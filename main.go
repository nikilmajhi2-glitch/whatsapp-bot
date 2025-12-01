
package main

import (
        "context"
        "encoding/json"
        "fmt"
        "log"
        "net/http"
        "os"
        "os/signal"
        "strings"
        "sync"
        "syscall"
        "time"

        _ "github.com/mattn/go-sqlite3"
        "github.com/gorilla/mux"
        "go.mau.fi/whatsmeow"
        "go.mau.fi/whatsmeow/proto/waE2E"
        "go.mau.fi/whatsmeow/store/sqlstore"
        "go.mau.fi/whatsmeow/types"
        "go.mau.fi/whatsmeow/types/events"
        waLog "go.mau.fi/whatsmeow/util/log"
        "google.golang.org/protobuf/proto"
)

// Device represents a WhatsApp device
type Device struct {
        ID           string              `json:"id"`
        PhoneNumber  string              `json:"phoneNumber"`
        Connected    bool                `json:"connected"`
        IsPairing    bool                `json:"isPairing"`
        PairingCode  string              `json:"pairingCode"`
        MessagesSent int                 `json:"messagesSent"`
        LastUsed     time.Time           `json:"lastUsed"`
        Client       *whatsmeow.Client   `json:"-"`
        UserID       string              `json:"userId"`       // Added for earning app
        CustomID     string              `json:"customId"`     // Added for earning app
}

// Campaign represents a bulk messaging campaign
type Campaign struct {
        ID        string    `json:"id"`
        Name      string    `json:"name"`
        Status    string    `json:"status"` // active, paused, completed, cancelled
        DeviceID  string    `json:"deviceId"`
        Message   string    `json:"message"`
        Total     int       `json:"total"`
        Sent      int       `json:"sent"`
        Failed    int       `json:"failed"`
        CreatedAt time.Time `json:"createdAt"`
        UpdatedAt time.Time `json:"updatedAt"`
}

// Message represents a single message
type Message struct {
        ID          string     `json:"id"`
        CampaignID  string     `json:"campaignId"`
        DeviceID    string     `json:"deviceId"`
        PhoneNumber string     `json:"phoneNumber"`
        MessageText string     `json:"messageText"`
        Status      string     `json:"status"` // pending, sent, failed, cancelled
        Timestamp   time.Time  `json:"timestamp"`
        SentAt      *time.Time `json:"sentAt,omitempty"`
}

// Earning App Request Types
type AddDeviceRequest struct {
        UserID      string `json:"userId"`
        PhoneNumber string `json:"phoneNumber"`
        CustomID    string `json:"customId"`
        DeviceGuid  string `json:"deviceGuid"`
}

type MessageWebhook struct {
        DeviceID    string `json:"deviceId"`
        UserID      string `json:"userId"`
        FromNumber  string `json:"fromNumber"`
        MessageText string `json:"messageText"`
        Timestamp   int64  `json:"timestamp"`
}

// WhatsAppManager manages multiple WhatsApp devices
type WhatsAppManager struct {
        devices    map[string]*Device
        campaigns  map[string]*Campaign
        messages   []Message
        mutex      sync.RWMutex
        container  *sqlstore.Container
        ctx        context.Context
        msgQueue   chan Message
        stopQueue  chan bool
        apiKey     string
}

// Request/Response types
type LoginRequest struct {
        PhoneNumber string `json:"phoneNumber"`
}

type LogoutRequest struct {
        DeviceID string `json:"deviceId"`
}

type SendMessageRequest struct {
        DeviceID    string `json:"deviceId"`
        PhoneNumber string `json:"phoneNumber"`
        Message     string `json:"message"`
}

type BulkMessageRequest struct {
        DeviceID     string   `json:"deviceId"`
        CampaignName string   `json:"campaignName"`
        Message      string   `json:"message"`
        PhoneNumbers []string `json:"phoneNumbers"`
}

type CampaignActionRequest struct {
        CampaignID string `json:"campaignId"`
}

func NewWhatsAppManager() *WhatsAppManager {
        ctx := context.Background()

        // Setup database
        dbLog := waLog.Stdout("Database", "ERROR", true) // Only show errors
        container, err := sqlstore.New(ctx, "sqlite3", "file:whatsapp.db?_foreign_keys=on", dbLog)
        if err != nil {
                log.Fatal("Failed to create database:", err)
        }

        wm := &WhatsAppManager{
                devices:   make(map[string]*Device),
                campaigns: make(map[string]*Campaign),
                messages:  make([]Message, 0),
                container: container,
                ctx:       ctx,
                msgQueue:  make(chan Message, 1000),
                stopQueue: make(chan bool),
                apiKey:    getEnv("API_KEY", "your-secret-api-key-2024"),
        }

        // Start message queue processor
        go wm.processMessageQueue()

        return wm
}

func (wm *WhatsAppManager) processMessageQueue() {
        for {
                select {
                case message := <-wm.msgQueue:
                        wm.sendMessage(message)
                        // Delay between messages to avoid spam detection
                        time.Sleep(3 * time.Second)
                case <-wm.stopQueue:
                        return
                }
        }
}

func (wm *WhatsAppManager) connectDevice(phoneNumber string) (*Device, error) {
        wm.mutex.Lock()
        defer wm.mutex.Unlock()

        // Create new device store
        deviceStore := wm.container.NewDevice()

        // Create client with minimal logging
        clientLog := waLog.Stdout("Client-"+phoneNumber, "ERROR", true) // Only show errors
        client := whatsmeow.NewClient(deviceStore, clientLog)

        device := &Device{
                ID:          phoneNumber,
                PhoneNumber: phoneNumber,
                Connected:   false,
                IsPairing:   true,
                Client:      client,
                LastUsed:    time.Now(),
        }

        // Add event handler
        client.AddEventHandler(func(evt interface{}) {
                switch v := evt.(type) {
                case *events.Connected:
                        log.Printf("✅ Device %s connected", phoneNumber)
                        wm.mutex.Lock()
                        device.Connected = true
                        device.IsPairing = false
                        wm.mutex.Unlock()
                case *events.Disconnected:
                        log.Printf("❌ Device %s disconnected", phoneNumber)
                        wm.mutex.Lock()
                        device.Connected = false
                        wm.mutex.Unlock()
                case *events.Message:
                        // Handle incoming messages for earning app
                        if !v.Info.IsFromMe && device.UserID != "" {
                                wm.handleIncomingMessage(device, v)
                        }
                }
        })

        // Connect to WhatsApp
        err := client.Connect()
        if err != nil {
                return nil, fmt.Errorf("failed to connect: %v", err)
        }

        // Generate pairing code
        code, err := client.PairPhone(wm.ctx, phoneNumber, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
        if err != nil {
                return nil, fmt.Errorf("failed to generate pairing code: %v", err)
        }

        device.PairingCode = code
        wm.devices[phoneNumber] = device

        // Wait for pairing in background
        go func() {
                for i := 0; i < 120; i++ { // Wait up to 2 minutes
                        time.Sleep(1 * time.Second)
                        if client.Store.ID != nil {
                                wm.mutex.Lock()
                                device.Connected = true
                                device.IsPairing = false
                                device.PairingCode = ""
                                wm.mutex.Unlock()
                                log.Printf("✅ Device %s paired successfully", phoneNumber)
                                break
                        }
                }
        }()

        return device, nil
}

// NEW: Connect device for earning app with user info
func (wm *WhatsAppManager) connectEarningDevice(phoneNumber, userID, customID string) (*Device, error) {
        device, err := wm.connectDevice(phoneNumber)
        if err != nil {
                return nil, err
        }

        // Add user info for earning tracking
        wm.mutex.Lock()
        device.UserID = userID
        device.CustomID = customID
        wm.mutex.Unlock()

        return device, nil
}

// NEW: Handle incoming messages for earning app
func (wm *WhatsAppManager) handleIncomingMessage(device *Device, msg *events.Message) {
        if device.UserID == "" {
                return // Not an earning app device
        }

        // Extract message text
        var messageText string
        if msg.Message.GetConversation() != "" {
                messageText = msg.Message.GetConversation()
        } else if msg.Message.GetExtendedTextMessage() != nil {
                messageText = msg.Message.GetExtendedTextMessage().GetText()
        }

        // Create webhook payload
        webhook := MessageWebhook{
                DeviceID:    device.ID,
                UserID:      device.UserID,
                FromNumber:  msg.Info.Sender.User,
                MessageText: messageText,
                Timestamp:   msg.Info.Timestamp.Unix(),
        }

        // Send webhook to Firebase/earning system
        go wm.sendMessageWebhook(webhook)

        log.Printf("💰 Message received for user %s (%s): +₹0.63", device.CustomID, device.UserID)
}

// NEW: Send webhook to earning system
func (wm *WhatsAppManager) sendMessageWebhook(webhook MessageWebhook) {
        // In a real implementation, this would send to your Firebase Functions
        // For now, just log the earning event
        log.Printf("📤 Webhook: User %s earned ₹0.63 from message", webhook.UserID)
        
        // TODO: Send HTTP request to your Firebase function to update user balance
        // This would be something like:
        // POST https://your-firebase-function.com/updateUserEarnings
        // Body: webhook data
}

func (wm *WhatsAppManager) disconnectDevice(deviceID string) error {
        wm.mutex.Lock()
        defer wm.mutex.Unlock()

        device, exists := wm.devices[deviceID]
        if !exists {
                return fmt.Errorf("device not found")
        }

        if device.Client != nil {
                device.Client.Disconnect()
        }

        delete(wm.devices, deviceID)
        return nil
}

func (wm *WhatsAppManager) getAvailableDevice(preferredDeviceID string) *Device {
        wm.mutex.RLock()
        defer wm.mutex.RUnlock()

        // If specific device requested
        if preferredDeviceID != "" {
                if device, exists := wm.devices[preferredDeviceID]; exists && device.Connected {
                        return device
                }
        }

        // Find device with least messages sent (load balancing)
        var bestDevice *Device
        for _, device := range wm.devices {
                if device.Connected {
                        if bestDevice == nil || device.MessagesSent < bestDevice.MessagesSent {
                                bestDevice = device
                        }
                }
        }

        return bestDevice
}

func (wm *WhatsAppManager) sendMessage(message Message) error {
        device := wm.getAvailableDevice(message.DeviceID)
        if device == nil {
                wm.updateMessageStatus(message.ID, "failed")
                return fmt.Errorf("no available device")
        }

        // Parse phone number
        jid, err := types.ParseJID(message.PhoneNumber + "@s.whatsapp.net")
        if err != nil {
                wm.updateMessageStatus(message.ID, "failed")
                return fmt.Errorf("invalid phone number: %v", err)
        }

        // Create message
        msg := &waE2E.Message{
                Conversation: proto.String(message.MessageText),
        }

        // Send message
        _, err = device.Client.SendMessage(wm.ctx, jid, msg)
        if err != nil {
                wm.updateMessageStatus(message.ID, "failed")
                log.Printf("❌ Failed to send to %s: %v", message.PhoneNumber, err)
                return fmt.Errorf("failed to send message: %v", err)
        }

        // Update statistics
        wm.mutex.Lock()
        device.MessagesSent++
        device.LastUsed = time.Now()
        wm.mutex.Unlock()

        wm.updateMessageStatus(message.ID, "sent")
        log.Printf("✅ Message sent to %s via device %s", message.PhoneNumber, device.PhoneNumber)

        return nil
}

func (wm *WhatsAppManager) updateMessageStatus(messageID, status string) {
        wm.mutex.Lock()
        defer wm.mutex.Unlock()

        for i := range wm.messages {
                if wm.messages[i].ID == messageID {
                        wm.messages[i].Status = status
                        if status == "sent" {
                                now := time.Now()
                                wm.messages[i].SentAt = &now
                        }
                        break
                }
        }
}

func (wm *WhatsAppManager) createCampaign(name, deviceID, message string, phoneNumbers []string) (*Campaign, error) {
        campaignID := fmt.Sprintf("campaign_%d", time.Now().UnixNano())

        campaign := &Campaign{
                ID:        campaignID,
                Name:      name,
                Status:    "active",
                DeviceID:  deviceID,
                Message:   message,
                Total:     len(phoneNumbers),
                Sent:      0,
                Failed:    0,
                CreatedAt: time.Now(),
                UpdatedAt: time.Now(),
        }

        wm.mutex.Lock()
        wm.campaigns[campaignID] = campaign
        wm.mutex.Unlock()

        // Queue messages
        for _, phone := range phoneNumbers {
                messageID := fmt.Sprintf("msg_%d_%s", time.Now().UnixNano(), phone)
                msg := Message{
                        ID:          messageID,
                        CampaignID:  campaignID,
                        DeviceID:    deviceID,
                        PhoneNumber: phone,
                        MessageText: message,
                        Status:      "pending",
                        Timestamp:   time.Now(),
                }

                wm.mutex.Lock()
                wm.messages = append(wm.messages, msg)
                wm.mutex.Unlock()

                // Queue for sending
                select {
                case wm.msgQueue <- msg:
                default:
                        log.Printf("⚠️ Message queue full, skipping message to %s", phone)
                }
        }

        return campaign, nil
}

func (wm *WhatsAppManager) getStatus() map[string]interface{} {
        wm.mutex.RLock()
        defer wm.mutex.RUnlock()

        devices := make([]*Device, 0, len(wm.devices))
        for _, device := range wm.devices {
                devices = append(devices, &Device{
                        ID:           device.ID,
                        PhoneNumber:  device.PhoneNumber,
                        Connected:    device.Connected,
                        IsPairing:    device.IsPairing,
                        PairingCode:  device.PairingCode,
                        MessagesSent: device.MessagesSent,
                        LastUsed:     device.LastUsed,
                        UserID:       device.UserID,
                        CustomID:     device.CustomID,
                })
        }

        return map[string]interface{}{
                "devices":   devices,
                "total":     len(devices),
                "connected": len(devices),
        }
}

// Middleware for API key validation
func (wm *WhatsAppManager) validateAPIKey(next http.HandlerFunc) http.HandlerFunc {
        return func(w http.ResponseWriter, r *http.Request) {
                apiKey := r.Header.Get("X-API-Key")
                if apiKey != wm.apiKey {
                        http.Error(w, "Unauthorized: Invalid API key", http.StatusUnauthorized)
                        return
                }
                next(w, r)
        }
}

// Helper function to get environment variables
func getEnv(key, fallback string) string {
        if value, exists := os.LookupEnv(key); exists {
                return value
        }
        return fallback
}

var manager *WhatsAppManager

func main() {
        fmt.Println("======================================================================")
        fmt.Println("🚀 WHATSAPP MULTI-DEVICE SYSTEM + EARNING APP API")
        fmt.Println("======================================================================")
        fmt.Println("✅ System Status: INITIALIZING...")

        manager = NewWhatsAppManager()

        fmt.Println("✅ Database: CONNECTED")
        fmt.Println("✅ Multi-Device Support: ENABLED")
        fmt.Println("✅ Earning App API: ENABLED")
        fmt.Println("✅ Load Balancing: ACTIVE")
        fmt.Println("✅ Campaign Management: READY")
        fmt.Println("✅ Message Tracking: ENABLED")
        fmt.Println("✅ Webhook System: ACTIVE")
        fmt.Println("✅ Web Dashboard: STARTING...")
        fmt.Println("======================================================================")

        port := getEnv("PORT", "8080")
        fmt.Printf("🌐 Server running on port: %s\n", port)
        if port == "8080" {
                fmt.Println("🌐 Dashboard: http://localhost:8080")
        }
        fmt.Println("======================================================================")

        // Setup HTTP server
        router := mux.NewRouter()

        // Enable CORS
        router.Use(func(next http.Handler) http.Handler {
                return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                        w.Header().Set("Access-Control-Allow-Origin", "*")
                        w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
                        w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
                        
                        if r.Method == "OPTIONS" {
                                w.WriteHeader(http.StatusOK)
                                return
                        }
                        
                        next.ServeHTTP(w, r)
                })
        })

        // Serve HTML dashboard
        router.HandleFunc("/", serveHTML).Methods("GET")
        router.HandleFunc("/health", handleHealth).Methods("GET")

        // Original WhatsApp Bot API Routes
        api := router.PathPrefix("/api").Subrouter()

        // Device management
        api.HandleFunc("/whatsapp/login", handleLogin).Methods("POST")
        api.HandleFunc("/whatsapp/logout", handleLogout).Methods("POST")
        api.HandleFunc("/whatsapp/status", handleStatus).Methods("GET")

        // Messaging
        api.HandleFunc("/whatsapp/send", handleSendSingle).Methods("POST")
        api.HandleFunc("/whatsapp/bulk", handleSendBulk).Methods("POST")

        // Campaign management
        api.HandleFunc("/campaigns", handleGetCampaigns).Methods("GET")
        api.HandleFunc("/campaigns/stop", handleStopCampaign).Methods("POST")
        api.HandleFunc("/campaigns/start", handleStartCampaign).Methods("POST")
        api.HandleFunc("/campaigns/delete", handleDeleteCampaign).Methods("POST")

        // Message management
        api.HandleFunc("/messages", handleGetMessages).Methods("GET")
        api.HandleFunc("/messages/delete", handleDeleteMessage).Methods("POST")

        // NEW: Earning App API Routes (Protected with API Key)
        earningAPI := router.PathPrefix("/api/earning").Subrouter()
        
        // Device management for earning app
        earningAPI.HandleFunc("/add-device", manager.validateAPIKey(handleAddDevice)).Methods("POST")
        earningAPI.HandleFunc("/device-status/{deviceId}", manager.validateAPIKey(handleDeviceStatus)).Methods("GET")
        earningAPI.HandleFunc("/remove-device/{deviceId}", manager.validateAPIKey(handleRemoveDevice)).Methods("DELETE")
        
        // Message webhook (for receiving message notifications)
        earningAPI.HandleFunc("/webhook/message", manager.validateAPIKey(handleMessageWebhook)).Methods("POST")

        // Start server
        srv := &http.Server{
                Handler: router,
                Addr:    ":" + port,
        }

        go func() {
                if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                        log.Fatalf("Server failed: %v", err)
                }
        }()

        // Wait for interrupt
        c := make(chan os.Signal, 1)
        signal.Notify(c, os.Interrupt, syscall.SIGTERM)
        <-c

        fmt.Println("\n👋 Shutting down...")
        manager.stopQueue <- true
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()
        srv.Shutdown(ctx)
}

// Original HTTP Handlers (unchanged)
func handleLogin(w http.ResponseWriter, r *http.Request) {
        var req LoginRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        device, err := manager.connectDevice(req.PhoneNumber)
        if err != nil {
                http.Error(w, err.Error(), http.StatusInternalServerError)
                return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
                "success":     true,
                "pairingCode": device.PairingCode,
                "deviceId":    device.ID,
        })
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
        var req LogoutRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        if err := manager.disconnectDevice(req.DeviceID); err != nil {
                http.Error(w, err.Error(), http.StatusInternalServerError)
                return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
        status := manager.getStatus()
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(status)
}

func handleSendSingle(w http.ResponseWriter, r *http.Request) {
        var req SendMessageRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
        message := Message{
                ID:          messageID,
                DeviceID:    req.DeviceID,
                PhoneNumber: req.PhoneNumber,
                MessageText: req.Message,
                Status:      "pending",
                Timestamp:   time.Now(),
        }

        manager.mutex.Lock()
        manager.messages = append(manager.messages, message)
        manager.mutex.Unlock()

        // Queue message
        select {
        case manager.msgQueue <- message:
                w.Header().Set("Content-Type", "application/json")
                json.NewEncoder(w).Encode(map[string]bool{"success": true})
        default:
                http.Error(w, "Message queue full", http.StatusServiceUnavailable)
        }
}

func handleSendBulk(w http.ResponseWriter, r *http.Request) {
        var req BulkMessageRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        campaign, err := manager.createCampaign(req.CampaignName, req.DeviceID, req.Message, req.PhoneNumbers)
        if err != nil {
                http.Error(w, err.Error(), http.StatusInternalServerError)
                return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
                "success":    true,
                "campaignId": campaign.ID,
                "total":      campaign.Total,
        })
}

func handleGetCampaigns(w http.ResponseWriter, r *http.Request) {
        manager.mutex.RLock()
        campaigns := make([]*Campaign, 0, len(manager.campaigns))
        for _, campaign := range manager.campaigns {
                campaigns = append(campaigns, campaign)
        }
        manager.mutex.RUnlock()

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(campaigns)
}

func handleStopCampaign(w http.ResponseWriter, r *http.Request) {
        var req CampaignActionRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        manager.mutex.Lock()
        if campaign, exists := manager.campaigns[req.CampaignID]; exists {
                campaign.Status = "stopped"
                campaign.UpdatedAt = time.Now()
        }
        manager.mutex.Unlock()

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleStartCampaign(w http.ResponseWriter, r *http.Request) {
        var req CampaignActionRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        manager.mutex.Lock()
        if campaign, exists := manager.campaigns[req.CampaignID]; exists {
                campaign.Status = "active"
                campaign.UpdatedAt = time.Now()
        }
        manager.mutex.Unlock()

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleDeleteCampaign(w http.ResponseWriter, r *http.Request) {
        var req CampaignActionRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        manager.mutex.Lock()
        delete(manager.campaigns, req.CampaignID)
        // Also remove related messages
        newMessages := make([]Message, 0)
        for _, msg := range manager.messages {
                if msg.CampaignID != req.CampaignID {
                        newMessages = append(newMessages, msg)
                }
        }
        manager.messages = newMessages
        manager.mutex.Unlock()

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleGetMessages(w http.ResponseWriter, r *http.Request) {
        manager.mutex.RLock()
        messages := make([]Message, len(manager.messages))
        copy(messages, manager.messages)
        manager.mutex.RUnlock()

        // Limit to last 1000 messages for performance
        if len(messages) > 1000 {
                messages = messages[len(messages)-1000:]
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(messages)
}

func handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
        var req struct {
                MessageID string `json:"messageId"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        manager.mutex.Lock()
        newMessages := make([]Message, 0)
        for _, msg := range manager.messages {
                if msg.ID != req.MessageID {
                        newMessages = append(newMessages, msg)
                }
        }
        manager.messages = newMessages
        manager.mutex.Unlock()

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
                "status":    "healthy",
                "timestamp": time.Now().Unix(),
                "devices":   len(manager.devices),
        })
}

// NEW: Earning App API Handlers
func handleAddDevice(w http.ResponseWriter, r *http.Request) {
        var req AddDeviceRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        // Validate phone number format
        phoneNumber := strings.TrimPrefix(req.PhoneNumber, "+91")
        if len(phoneNumber) != 10 {
                http.Error(w, "Invalid phone number format", http.StatusBadRequest)
                return
        }

        // Connect device with user info
        device, err := manager.connectEarningDevice(phoneNumber, req.UserID, req.CustomID)
        if err != nil {
                http.Error(w, fmt.Sprintf("Failed to connect device: %v", err), http.StatusInternalServerError)
                return
        }

        log.Printf("📱 New earning device added: %s for user %s (%s)", phoneNumber, req.CustomID, req.UserID)

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
                "success":     true,
                "deviceId":    device.ID,
                "pairingCode": device.PairingCode,
                "message":     "Enter this pairing code in WhatsApp",
        })
}

func handleDeviceStatus(w http.ResponseWriter, r *http.Request) {
        vars := mux.Vars(r)
        deviceID := vars["deviceId"]

        manager.mutex.RLock()
        device, exists := manager.devices[deviceID]
        manager.mutex.RUnlock()

        if !exists {
                http.Error(w, "Device not found", http.StatusNotFound)
                return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
                "deviceId":      device.ID,
                "connected":     device.Connected,
                "isPairing":     device.IsPairing,
                "pairingCode":   device.PairingCode,
                "messagesSent":  device.MessagesSent,
                "lastUsed":      device.LastUsed,
        })
}

func handleRemoveDevice(w http.ResponseWriter, r *http.Request) {
        vars := mux.Vars(r)
        deviceID := vars["deviceId"]

        err := manager.disconnectDevice(deviceID)
        if err != nil {
                http.Error(w, err.Error(), http.StatusInternalServerError)
                return
        }

        log.Printf("📱 Device removed: %s", deviceID)

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
                "success": true,
                "message": "Device removed successfully",
        })
}

func handleMessageWebhook(w http.ResponseWriter, r *http.Request) {
        var webhook MessageWebhook
        if err := json.NewDecoder(r.Body).Decode(&webhook); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
        }

        // Process the webhook (update user earnings, etc.)
        log.Printf("💰 Webhook received: User %s earned from message", webhook.UserID)

        // Here you would typically update the user's balance in your database
        // For now, just acknowledge the webhook

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
                "success": true,
                "message": "Webhook processed successfully",
        })
}

func serveHTML(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "text/html")
        w.Write([]byte(getHTMLContent()))
}

func getHTMLContent() string {
        html, err := os.ReadFile("dashboard.html")
        if err != nil {
                return `<!DOCTYPE html>
<html>
<head><title>WhatsApp Multi-Device System</title></head>
<body>
<h1>🚀 WhatsApp Multi-Device System + Earning App API</h1>
<h2>📊 System Status</h2>
<p>✅ WhatsApp Bot API: Active</p>
<p>✅ Earning App API: Active</p>
<p>✅ Multi-Device Support: Enabled</p>
<p>✅ Message Tracking: Enabled</p>

<h2>🔗 API Endpoints</h2>
<h3>Earning App APIs:</h3>
<ul>
<li><strong>POST</strong> /api/earning/add-device - Add WhatsApp device</li>
<li><strong>GET</strong> /api/earning/device-status/{deviceId} - Check device status</li>
<li><strong>DELETE</strong> /api/earning/remove-device/{deviceId} - Remove device</li>
<li><strong>POST</strong> /api/earning/webhook/message - Message webhook</li>
</ul>

<h3>WhatsApp Bot APIs:</h3>
<ul>
<li><strong>POST</strong> /api/whatsapp/login - Connect WhatsApp</li>
<li><strong>POST</strong> /api/whatsapp/send - Send single message</li>
<li><strong>POST</strong> /api/whatsapp/bulk - Send bulk messages</li>
<li><strong>GET</strong> /api/whatsapp/status - Get status</li>
</ul>

<p><strong>API Key Required:</strong> Add <code>X-API-Key</code> header for earning app APIs</p>
</body>
</html>`
        }
        return string(html)
}

