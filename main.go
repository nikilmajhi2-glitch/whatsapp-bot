package main

import (
	"context"
	"database/sql"
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

	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// --- Structs ---

type Device struct {
	ID           string            `json:"id"`
	PhoneNumber  string            `json:"phoneNumber"`
	Connected    bool              `json:"connected"`
	IsPairing    bool              `json:"isPairing"`
	PairingCode  string            `json:"pairingCode"`
	MessagesSent int               `json:"messagesSent"`
	LastUsed     time.Time         `json:"lastUsed"`
	Client       *whatsmeow.Client `json:"-"`
	UserID       string            `json:"userId"`
	CustomID     string            `json:"customId"`
}

type Message struct {
	ID          string     `json:"id"`
	DeviceID    string     `json:"deviceId"`
	PhoneNumber string     `json:"phoneNumber"`
	MessageText string     `json:"messageText"`
	Status      string     `json:"status"`
	Timestamp   time.Time  `json:"timestamp"`
	SentAt      *time.Time `json:"sentAt,omitempty"`
}

type BulkRequest struct {
	DeviceID     string   `json:"deviceId"`
	PhoneNumbers []string `json:"phoneNumbers"`
	Message      string   `json:"message"`
}

type AddDeviceRequest struct {
	UserID      string `json:"userId"`
	PhoneNumber string `json:"phoneNumber"`
	CustomID    string `json:"customId"`
}

type WhatsAppManager struct {
	devices   map[string]*Device
	mutex     sync.RWMutex
	container *sqlstore.Container
	db        *sql.DB
	ctx       context.Context
	msgQueue  chan Message
	apiKey    string
}

// --- Initialization ---

func NewWhatsAppManager() *WhatsAppManager {
	ctx := context.Background()
	dbLog := waLog.Stdout("Database", "ERROR", true)
	
	container, err := sqlstore.New(ctx, "sqlite3", "file:whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		log.Fatal("Failed store:", err)
	}

	db, err := sql.Open("sqlite3", "file:whatsapp.db?_foreign_keys=on")
	if err != nil {
		log.Fatal("Failed db:", err)
	}

	wm := &WhatsAppManager{
		devices:   make(map[string]*Device),
		container: container,
		db:        db,
		ctx:       ctx,
		msgQueue:  make(chan Message, 2000),
		apiKey:    getEnv("API_KEY", "your-secret-api-key-2024"),
	}

	wm.initDB()
	wm.reconnectDevices()
	go wm.processMessageQueue()
	return wm
}

func (wm *WhatsAppManager) initDB() {
	query := `
	CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY, device_id TEXT, phone_number TEXT, 
		message_text TEXT, status TEXT, timestamp DATETIME, sent_at DATETIME
	);
	CREATE TABLE IF NOT EXISTS earning_users (
		phone_number TEXT PRIMARY KEY, user_id TEXT, custom_id TEXT
	);
	`
	_, err := wm.db.Exec(query)
	if err != nil {
		log.Fatal("DB Init failed:", err)
	}
}

func (wm *WhatsAppManager) reconnectDevices() {
	devices, err := wm.container.GetAllDevices(wm.ctx)
	if err != nil {
		return
	}
	
	for _, d := range devices {
		if d.ID == nil { continue }
		phoneNumber := d.ID.User 
		
		var userID, customID string
		wm.db.QueryRow("SELECT user_id, custom_id FROM earning_users WHERE phone_number = ?", phoneNumber).Scan(&userID, &customID)

		clientLog := waLog.Stdout("Client-"+phoneNumber, "ERROR", true)
		client := whatsmeow.NewClient(d, clientLog)
		
		device := &Device{
			ID:          phoneNumber,
			PhoneNumber: phoneNumber,
			Connected:   false,
			Client:      client,
			UserID:      userID,
			CustomID:    customID,
			LastUsed:    time.Now(),
		}

		wm.registerHandlers(client, device)
		client.Connect()
		
		wm.mutex.Lock()
		wm.devices[phoneNumber] = device
		wm.mutex.Unlock()
	}
}

func (wm *WhatsAppManager) connectDevice(phoneNumber string, userID, customID string) (*Device, error) {
	wm.mutex.Lock()
	defer wm.mutex.Unlock()

	if dev, ok := wm.devices[phoneNumber]; ok && dev.Connected {
		return dev, nil
	}

	deviceStore := wm.container.NewDevice()
	clientLog := waLog.Stdout("Client-"+phoneNumber, "ERROR", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	device := &Device{
		ID:          phoneNumber,
		PhoneNumber: phoneNumber,
		Connected:   false,
		IsPairing:   true,
		Client:      client,
		LastUsed:    time.Now(),
		UserID:      userID,
		CustomID:    customID,
	}

	wm.registerHandlers(client, device)

	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("connect failed: %v", err)
	}

	code, err := client.PairPhone(wm.ctx, phoneNumber, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return nil, fmt.Errorf("pairing failed: %v", err)
	}

	device.PairingCode = code
	wm.devices[phoneNumber] = device

	if userID != "" {
		wm.db.Exec("INSERT OR REPLACE INTO earning_users (phone_number, user_id, custom_id) VALUES (?, ?, ?)", phoneNumber, userID, customID)
	}

	go wm.monitorPairing(device)
	return device, nil
}

func (wm *WhatsAppManager) registerHandlers(client *whatsmeow.Client, device *Device) {
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Connected:
			wm.mutex.Lock()
			device.Connected = true
			device.IsPairing = false
			wm.mutex.Unlock()
		case *events.Disconnected:
			wm.mutex.Lock()
			device.Connected = false
			wm.mutex.Unlock()
		case *events.Message:
			if !v.Info.IsFromMe && device.UserID != "" {
				wm.handleIncomingMessage(device, v)
			}
		case *events.PairSuccess:
			wm.mutex.Lock()
			device.Connected = true
			device.IsPairing = false
			device.PairingCode = ""
			wm.mutex.Unlock()
		}
	})
}

func (wm *WhatsAppManager) monitorPairing(device *Device) {
	for i := 0; i < 120; i++ {
		time.Sleep(1 * time.Second)
		if device.Client.Store.ID != nil {
			wm.mutex.Lock()
			device.Connected = true
			device.IsPairing = false
			device.PairingCode = ""
			wm.mutex.Unlock()
			return
		}
	}
}

func (wm *WhatsAppManager) handleIncomingMessage(device *Device, msg *events.Message) {
	var messageText string
	if msg.Message.GetConversation() != "" {
		messageText = msg.Message.GetConversation()
	} else if msg.Message.GetExtendedTextMessage() != nil {
		messageText = msg.Message.GetExtendedTextMessage().GetText()
	}

	wm.db.Exec("INSERT INTO messages (id, device_id, phone_number, message_text, status, timestamp) VALUES (?,?,?,?,?,?)",
		fmt.Sprintf("in_%d", msg.Info.Timestamp.UnixNano()), device.ID, msg.Info.Sender.User, messageText, "received", time.Now())

	log.Printf("💰 Msg received on %s: %s", device.PhoneNumber, messageText)
}

func (wm *WhatsAppManager) processMessageQueue() {
	for msg := range wm.msgQueue {
		wm.sendActualMessage(msg)
		time.Sleep(1 * time.Second)
	}
}

func (wm *WhatsAppManager) sendActualMessage(message Message) {
	wm.mutex.RLock()
	device, exists := wm.devices[message.DeviceID]
	wm.mutex.RUnlock()

	if !exists || !device.Connected {
		for _, d := range wm.devices {
			if d.Connected {
				device = d
				break
			}
		}
	}

	if device == nil || !device.Connected {
		wm.updateMsgStatus(message.ID, "failed")
		return
	}

	jid, _ := types.ParseJID(message.PhoneNumber + "@s.whatsapp.net")
	waMsg := &waE2E.Message{Conversation: proto.String(message.MessageText)}

	_, err := device.Client.SendMessage(wm.ctx, jid, waMsg)
	if err != nil {
		wm.updateMsgStatus(message.ID, "failed")
		return
	}

	wm.mutex.Lock()
	device.MessagesSent++
	wm.mutex.Unlock()
	wm.updateMsgStatus(message.ID, "sent")
}

func (wm *WhatsAppManager) updateMsgStatus(id, status string) {
	wm.db.Exec("UPDATE messages SET status = ?, sent_at = ? WHERE id = ?", status, time.Now(), id)
}

// --- API Handlers ---

func handleGetMessages(w http.ResponseWriter, r *http.Request) {
	rows, _ := manager.db.Query("SELECT id, device_id, phone_number, message_text, status, timestamp FROM messages ORDER BY timestamp DESC LIMIT 100")
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		rows.Scan(&m.ID, &m.DeviceID, &m.PhoneNumber, &m.MessageText, &m.Status, &m.Timestamp)
		messages = append(messages, m)
	}
	json.NewEncoder(w).Encode(messages)
}

func handleSendSingle(w http.ResponseWriter, r *http.Request) {
	var req Message
	json.NewDecoder(r.Body).Decode(&req)
	req.ID = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	req.Status = "pending"
	req.Timestamp = time.Now()
	manager.db.Exec("INSERT INTO messages (id, device_id, phone_number, message_text, status, timestamp) VALUES (?,?,?,?,?,?)",
		req.ID, req.DeviceID, req.PhoneNumber, req.MessageText, "pending", req.Timestamp)
	manager.msgQueue <- req
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleSendBulk(w http.ResponseWriter, r *http.Request) {
	var req BulkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", 400)
		return
	}
	go func() {
		for _, phone := range req.PhoneNumbers {
			phone = strings.TrimSpace(phone)
			if phone == "" { continue }
			msg := Message{
				ID:          fmt.Sprintf("bulk_%d_%s", time.Now().UnixNano(), phone),
				DeviceID:    req.DeviceID,
				PhoneNumber: phone,
				MessageText: req.Message,
				Status:      "pending",
				Timestamp:   time.Now(),
			}
			manager.db.Exec("INSERT INTO messages (id, device_id, phone_number, message_text, status, timestamp) VALUES (?,?,?,?,?,?)",
				msg.ID, msg.DeviceID, msg.PhoneNumber, msg.MessageText, "pending", msg.Timestamp)
			manager.msgQueue <- msg
		}
	}()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": fmt.Sprintf("Queued %d messages", len(req.PhoneNumbers))})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	manager.mutex.RLock()
	defer manager.mutex.RUnlock()
	list := make([]*Device, 0)
	for _, d := range manager.devices {
		list = append(list, d)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"devices": list})
}

func handleAddDevice(w http.ResponseWriter, r *http.Request) {
	var req AddDeviceRequest
	json.NewDecoder(r.Body).Decode(&req)
	phone := strings.TrimPrefix(req.PhoneNumber, "+")
	device, err := manager.connectDevice(phone, req.UserID, req.CustomID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "deviceId": device.ID, "pairingCode": device.PairingCode})
}

func handleRemoveDevice(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["deviceId"]
	manager.mutex.Lock()
	if dev, ok := manager.devices[id]; ok {
		if dev.Client != nil {
			dev.Client.Disconnect()
			dev.Client.Logout(manager.ctx)
		}
		jid, _ := types.ParseJID(id + "@s.whatsapp.net")
		manager.container.DeleteDevice(manager.ctx, &store.Device{ID: &jid}) 
		delete(manager.devices, id)
	}
	manager.mutex.Unlock()
	manager.db.Exec("DELETE FROM earning_users WHERE phone_number = ?", id)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

var manager *WhatsAppManager

func getEnv(key, fallback string) string {
	if v, exists := os.LookupEnv(key); exists { return v }
	return fallback
}

func main() {
	manager = NewWhatsAppManager()
	r := mux.NewRouter()
	
	// CORS
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == "OPTIONS" { w.WriteHeader(200); return }
			next.ServeHTTP(w, r)
		})
	})

	api := r.PathPrefix("/api").Subrouter()
	api.HandleFunc("/whatsapp/status", handleStatus).Methods("GET")
	api.HandleFunc("/whatsapp/send", handleSendSingle).Methods("POST")
	api.HandleFunc("/whatsapp/bulk", handleSendBulk).Methods("POST")
	api.HandleFunc("/messages", handleGetMessages).Methods("GET")
	
	earning := api.PathPrefix("/earning").Subrouter()
	earning.HandleFunc("/add-device", handleAddDevice).Methods("POST")
	earning.HandleFunc("/remove-device/{deviceId}", handleRemoveDevice).Methods("DELETE")

	// Admin Panel
	r.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "admin.html")
	})

	// Serve Static Files (Public folder)
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./public/")))

	port := getEnv("PORT", "8080")
	fmt.Printf("🚀 Server running. \n📱 User Panel: http://localhost:%s\n👮 Admin Panel: http://localhost:%s/admin\n", port, port)
	
	srv := &http.Server{Addr: ":" + port, Handler: r}
	go srv.ListenAndServe()
	
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	manager.db.Close()
	fmt.Println("Shutting down...")
}
