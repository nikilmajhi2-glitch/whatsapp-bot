package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
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

/* ---------------------------------------------------
   ADMIN LOGIN CONFIG (LOCAL BACKEND LOGIN)
--------------------------------------------------- */

var masterAdminEmail = "admin@rupeedesk.com"
var masterAdminPassword = "admin@6371" // Change anytime

// Admin Login API
func adminLogin(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&data)

	if data.Email == masterAdminEmail && data.Password == masterAdminPassword {
		http.SetCookie(w, &http.Cookie{
			Name:   "adminSession",
			Value:  "authenticated",
			MaxAge: 86400,
			Path:   "/",
		})
		w.Write([]byte(`{"status":"ok","role":"admin"}`))
		return
	}
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

func adminLoginCheck(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("adminSession")
	if err != nil || c.Value != "authenticated" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	w.Write([]byte(`{"status":"ok"}`))
}

/* ---------------------------------------------------
   SIMPLE SENDER DELAY (Semi-Fast)
   - ~1 second base delay between messages
   - small random jitter to avoid constant interval
--------------------------------------------------- */

func semiFastDelay() {
	// base 1 second + random 0-400ms jitter
	sleepMs := 1000 + rand.Intn(400)
	time.Sleep(time.Duration(sleepMs) * time.Millisecond)
}

// small humanize to vary content slightly
func humanize(msg string) string {
	variants := []string{
		msg,
		msg + " 🙂",
		"Hi! " + msg,
		msg + " 🙏",
	}
	return variants[rand.Intn(len(variants))]
}

/* ---------------------------------------------------
   STRUCTS
--------------------------------------------------- */

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

	CreatedAt time.Time `json:"createdAt"`
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

/* ---------------------------------------------------
   INITIALIZATION
--------------------------------------------------- */

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
/* ---------------------------------------------------
   RECONNECT / CONNECT / HANDLERS
--------------------------------------------------- */

func (wm *WhatsAppManager) reconnectDevices() {
	devices, err := wm.container.GetAllDevices(wm.ctx)
	if err != nil {
		return
	}

	for _, d := range devices {
		if d.ID == nil {
			continue
		}
		phoneNumber := d.ID.User

		var userID, customID string
		_ = wm.db.QueryRow("SELECT user_id, custom_id FROM earning_users WHERE phone_number = ?", phoneNumber).Scan(&userID, &customID)

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
			CreatedAt:   time.Now(),
		}

		wm.registerHandlers(client, device)
		_ = client.Connect()

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
		CreatedAt:   time.Now(),
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
		_, _ = wm.db.Exec("INSERT OR REPLACE INTO earning_users (phone_number, user_id, custom_id) VALUES (?, ?, ?)", phoneNumber, userID, customID)
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
		time.Sleep(time.Second)
		if device.Client != nil && device.Client.Store != nil && device.Client.Store.ID != nil {
			wm.mutex.Lock()
			device.Connected = true
			device.IsPairing = false
			device.PairingCode = ""
			wm.mutex.Unlock()
			return
		}
	}
}

/* ---------------------------------------------------
   MESSAGE HANDLING (SIMPLE QUEUE & SENDER)
--------------------------------------------------- */

func (wm *WhatsAppManager) handleIncomingMessage(device *Device, msg *events.Message) {
	var body string

	if msg.Message.GetConversation() != "" {
		body = msg.Message.GetConversation()
	} else if msg.Message.GetExtendedTextMessage() != nil {
		body = msg.Message.GetExtendedTextMessage().GetText()
	}

	_, _ = wm.db.Exec(
		"INSERT INTO messages (id, device_id, phone_number, message_text, status, timestamp) VALUES (?,?,?,?,?,?)",
		fmt.Sprintf("in_%d", msg.Info.Timestamp.UnixNano()),
		device.ID,
		msg.Info.Sender.User,
		body,
		"received",
		time.Now(),
	)
}

// processMessageQueue sends messages sequentially with semi-fast delay
func (wm *WhatsAppManager) processMessageQueue() {
	for msg := range wm.msgQueue {
		// quick validation
		if strings.TrimSpace(msg.PhoneNumber) == "" || strings.TrimSpace(msg.MessageText) == "" {
			wm.updateMsgStatus(msg.ID, "failed")
			continue
		}

		// pick device (prefer requested, else any connected)
		wm.mutex.RLock()
		dev := wm.devices[msg.DeviceID]
		wm.mutex.RUnlock()

		if dev == nil || !dev.Connected {
			// pick first connected device
			wm.mutex.RLock()
			for _, d := range wm.devices {
				if d.Connected {
					dev = d
					break
				}
			}
			wm.mutex.RUnlock()
		}

		if dev == nil || !dev.Connected {
			// no device available, mark failed and continue
			wm.updateMsgStatus(msg.ID, "failed")
			continue
		}

		// send
		ok := wm.sendActualMessageWithDevice(dev, msg)
		if ok {
			wm.updateMsgStatus(msg.ID, "sent")
		} else {
			wm.updateMsgStatus(msg.ID, "failed")
		}

		// semi-fast delay between sends
		semiFastDelay()
	}
}

func (wm *WhatsAppManager) sendActualMessageWithDevice(device *Device, message Message) bool {
	jid, err := types.ParseJID(strings.TrimSpace(message.PhoneNumber) + "@s.whatsapp.net")
	if err != nil {
		log.Printf("Invalid JID for %s: %v\n", message.PhoneNumber, err)
		return false
	}

	finalText := humanize(message.MessageText)
	waMsg := &waE2E.Message{Conversation: proto.String(finalText)}

	ctx, cancel := context.WithTimeout(wm.ctx, 25*time.Second)
	defer cancel()

	_, err = device.Client.SendMessage(ctx, jid, waMsg)
	if err != nil {
		// retry once after tiny backoff
		time.Sleep(500 * time.Millisecond)
		_, err2 := device.Client.SendMessage(wm.ctx, jid, waMsg)
		if err2 != nil {
			log.Printf("Send failed to %s: %v / retry: %v\n", message.PhoneNumber, err, err2)
			return false
		}
	}

	// on success update device stats
	wm.mutex.Lock()
	device.MessagesSent++
	device.LastUsed = time.Now()
	wm.mutex.Unlock()

	return true
}

func (wm *WhatsAppManager) updateMsgStatus(id, status string) {
	_, _ = wm.db.Exec("UPDATE messages SET status=?, sent_at=? WHERE id=?", status, time.Now(), id)
}
/* ---------------------------------------------------
   API HANDLERS (messages, devices)
--------------------------------------------------- */

func handleGetMessages(w http.ResponseWriter, r *http.Request) {
	rows, err := manager.db.Query(`
		SELECT id, device_id, phone_number, message_text, status, timestamp 
		FROM messages ORDER BY timestamp DESC LIMIT 200
	`)
	if err != nil {
		http.Error(w, "DB error", 500)
		return
	}
	defer rows.Close()

	var messages []Message

	for rows.Next() {
		var m Message
		_ = rows.Scan(&m.ID, &m.DeviceID, &m.PhoneNumber, &m.MessageText, &m.Status, &m.Timestamp)
		messages = append(messages, m)
	}

	_ = json.NewEncoder(w).Encode(messages)
}

func handleSendSingle(w http.ResponseWriter, r *http.Request) {
	var req Message
	_ = json.NewDecoder(r.Body).Decode(&req)

	req.ID = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	req.Status = "pending"
	req.Timestamp = time.Now()

	_, _ = manager.db.Exec(`
		INSERT INTO messages (id, device_id, phone_number, message_text, status, timestamp) 
		VALUES (?,?,?,?,?,?)
	`, req.ID, req.DeviceID, req.PhoneNumber, req.MessageText, req.Status, req.Timestamp)

	// push to the queue
	manager.msgQueue <- req

	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
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
			if phone == "" {
				continue
			}

			msg := Message{
				ID:          fmt.Sprintf("bulk_%d_%s", time.Now().UnixNano(), phone),
				DeviceID:    req.DeviceID,
				PhoneNumber: phone,
				MessageText: req.Message,
				Status:      "pending",
				Timestamp:   time.Now(),
			}

			_, _ = manager.db.Exec(`
				INSERT INTO messages (id, device_id, phone_number, message_text, status, timestamp)
				VALUES (?,?,?,?,?,?)
			`, msg.ID, msg.DeviceID, msg.PhoneNumber, msg.MessageText, msg.Status, msg.Timestamp)

			manager.msgQueue <- msg
		}
	}()

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Queued %d messages", len(req.PhoneNumbers)),
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	manager.mutex.RLock()
	defer manager.mutex.RUnlock()

	list := make([]*Device, 0)

	for _, d := range manager.devices {
		list = append(list, d)
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"devices": list})
}

func handleAddDevice(w http.ResponseWriter, r *http.Request) {
	var req AddDeviceRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	phone := strings.TrimPrefix(req.PhoneNumber, "+")
	device, err := manager.connectDevice(phone, req.UserID, req.CustomID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"deviceId":    device.ID,
		"pairingCode": device.PairingCode,
	})
}

func handleRemoveDevice(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["deviceId"]

	manager.mutex.Lock()

	if dev, ok := manager.devices[id]; ok {
		if dev.Client != nil {
			// Disconnect does not return a value
			dev.Client.Disconnect()
			_ = dev.Client.Logout(manager.ctx)
		}

		jid, _ := types.ParseJID(id + "@s.whatsapp.net")
		_ = manager.container.DeleteDevice(manager.ctx, &store.Device{ID: &jid})

		delete(manager.devices, id)
	}

	manager.mutex.Unlock()

	_, _ = manager.db.Exec("DELETE FROM earning_users WHERE phone_number=?", id)

	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

/* ---------------------------------------------------
   MAIN
--------------------------------------------------- */

var manager *WhatsAppManager

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func main() {
	// seed RNG for humanize and delays
	rand.Seed(time.Now().UnixNano())

	manager = NewWhatsAppManager()

	r := mux.NewRouter()

	// CORS
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == "OPTIONS" {
				w.WriteHeader(200)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	api := r.PathPrefix("/api").Subrouter()
	api.HandleFunc("/admin-login", adminLogin).Methods("POST")
	api.HandleFunc("/admin-login-check", adminLoginCheck).Methods("GET")

	api.HandleFunc("/whatsapp/status", handleStatus).Methods("GET")
	api.HandleFunc("/whatsapp/send", handleSendSingle).Methods("POST")
	api.HandleFunc("/whatsapp/bulk", handleSendBulk).Methods("POST")
	api.HandleFunc("/messages", handleGetMessages).Methods("GET")

	earning := api.PathPrefix("/earning").Subrouter()
	earning.HandleFunc("/add-device", handleAddDevice).Methods("POST")
	earning.HandleFunc("/remove-device/{deviceId}", handleRemoveDevice).Methods("DELETE")

	// Admin panel files
	r.PathPrefix("/admin/").Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("adminSession")
		if err != nil || c.Value != "authenticated" {
			http.Redirect(w, r, "/login.html", http.StatusFound)
			return
		}
		http.FileServer(http.Dir("./public/")).ServeHTTP(w, r)
	}))

	// Static
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./public/")))

	port := getEnv("PORT", "8080")
	fmt.Printf("\n🚀 Server Running")
	fmt.Printf("\n📱 User Panel : http://localhost:%s", port)
	fmt.Printf("\n👑 Admin Panel: http://localhost:%s/admin/admin.html\n\n", port)

	srv := &http.Server{Addr: ":" + port, Handler: r}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe(): %v", err)
		}
	}()

	// Shutdown handling
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("Shutting down...")
	_ = manager.db.Close()
	_ = srv.Close()
}