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
   STRONG ANTI-BAN SETTINGS (defaults)
--------------------------------------------------- */

// Global daily cap across all devices (sum)
var DailyLimit = 200

// Random per-message delay (seconds)
var MinDelay = 3
var MaxDelay = 7

// Extra delay when message contains link
var LinkMinDelay = 10
var LinkMaxDelay = 18

// Long break after N messages (simulates human pauses)
var BreakEvery = 12
var BreakMin = 35
var BreakMax = 75

// Silent hours (local server time) during which sending is blocked
var SilentStart = 1 // 1 AM
var SilentEnd = 6   // 6 AM

// RNG helper: sleep for randomized seconds between min and max (inclusive)
func randomDelay(min, max int) {
	if max <= min {
		time.Sleep(time.Duration(min) * time.Second)
		return
	}
	d := time.Duration(min+rand.Intn(max-min+1)) * time.Second
	time.Sleep(d)
}

// Human-like message variation to avoid identical payload detection.
func humanize(msg string) string {
	variants := []string{
		msg,
		msg + " 😊",
		"Hi! " + msg,
		msg + " 🙏",
		strings.Replace(msg, "Hello", "Hi", 1),
		strings.Replace(msg, "Hello", "Hey", 1),
	}
	return variants[rand.Intn(len(variants))]
}

/* ---------------------------------------------------
   ANTI-BAN CONFIG STORAGE (ONLY FOR ADMIN UI)
--------------------------------------------------- */

type AntiBanSettings struct {
	DailyLimit  int `json:"dailyLimit"`
	MinDelay    int `json:"minDelay"`
	MaxDelay    int `json:"maxDelay"`
	LinkMin     int `json:"linkMin"`
	LinkMax     int `json:"linkMax"`
	BreakEvery  int `json:"breakEvery"`
	BreakMin    int `json:"breakMin"`
	BreakMax    int `json:"breakMax"`
	SilentStart int `json:"silentStart"`
	SilentEnd   int `json:"silentEnd"`
}

var AntiBanConfig AntiBanSettings

func loadAntiBanConfig() {
	data, err := os.ReadFile("antiban.json")
	if err != nil {
		AntiBanConfig = AntiBanSettings{
			DailyLimit:  DailyLimit,
			MinDelay:    MinDelay,
			MaxDelay:    MaxDelay,
			LinkMin:     LinkMinDelay,
			LinkMax:     LinkMaxDelay,
			BreakEvery:  BreakEvery,
			BreakMin:    BreakMin,
			BreakMax:    BreakMax,
			SilentStart: SilentStart,
			SilentEnd:   SilentEnd,
		}
		_ = saveAntiBanConfig()
		return
	}
	_ = json.Unmarshal(data, &AntiBanConfig)

	// sync with globals only if values non-zero
	if AntiBanConfig.DailyLimit != 0 {
		DailyLimit = AntiBanConfig.DailyLimit
	}
	if AntiBanConfig.MinDelay != 0 {
		MinDelay = AntiBanConfig.MinDelay
	}
	if AntiBanConfig.MaxDelay != 0 {
		MaxDelay = AntiBanConfig.MaxDelay
	}
	if AntiBanConfig.LinkMin != 0 {
		LinkMinDelay = AntiBanConfig.LinkMin
	}
	if AntiBanConfig.LinkMax != 0 {
		LinkMaxDelay = AntiBanConfig.LinkMax
	}
	if AntiBanConfig.BreakEvery != 0 {
		BreakEvery = AntiBanConfig.BreakEvery
	}
	if AntiBanConfig.BreakMin != 0 {
		BreakMin = AntiBanConfig.BreakMin
	}
	if AntiBanConfig.BreakMax != 0 {
		BreakMax = AntiBanConfig.BreakMax
	}
	if AntiBanConfig.SilentStart != 0 {
		SilentStart = AntiBanConfig.SilentStart
	}
	if AntiBanConfig.SilentEnd != 0 {
		SilentEnd = AntiBanConfig.SilentEnd
	}
}

func saveAntiBanConfig() error {
	file, err := json.MarshalIndent(AntiBanConfig, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile("antiban.json", file, 0644)
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

	// NEW ANTI-BAN FIELDS (per-device)
	DailySent    int       `json:"dailySent"`
	LastResetDay int       `json:"lastResetDay"`
	Trust        int       `json:"trust"`
	CreatedAt    time.Time `json:"createdAt"`
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
	devices      map[string]*Device
	mutex        sync.RWMutex
	container    *sqlstore.Container
	db           *sql.DB
	ctx          context.Context
	msgQueue     chan Message
	apiKey       string
	DailySent    int
	LastResetDay int
	TrustScore   int
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
		devices:      make(map[string]*Device),
		container:    container,
		db:           db,
		ctx:          ctx,
		msgQueue:     make(chan Message, 3000),
		apiKey:       getEnv("API_KEY", "your-secret-api-key-2024"),
		DailySent:    0,
		LastResetDay: time.Now().Day(),
		TrustScore:   50,
	}

	loadAntiBanConfig()
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
   DEVICE WARM-UP & TRUST HELPERS
--------------------------------------------------- */

func warmUpLimit(device *Device) int {
	if device == nil {
		return 0
	}
	days := int(time.Since(device.CreatedAt).Hours() / 24)

	switch {
	case days <= 2:
		return 20
	case days <= 4:
		return 40
	case days <= 7:
		return 75
	case days <= 14:
		return 120
	default:
		return 200
	}
}

func (wm *WhatsAppManager) deviceCanSend(device *Device) bool {
	if device == nil {
		return false
	}
	now := time.Now()

	// Reset per-device daily counter at midnight
	if device.LastResetDay != now.Day() {
		device.DailySent = 0
		device.LastResetDay = now.Day()
		if device.Trust < 90 {
			device.Trust += 5
		}
	}

	// Warm-up limit
	if device.DailySent >= warmUpLimit(device) {
		return false
	}

	// If trust extremely low, block
	if device.Trust < 25 {
		return false
	}

	return true
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
			ID:           phoneNumber,
			PhoneNumber:  phoneNumber,
			Connected:    false,
			Client:       client,
			UserID:       userID,
			CustomID:     customID,
			LastUsed:     time.Now(),
			DailySent:    0,
			LastResetDay: time.Now().Day(),
			Trust:        50,
			CreatedAt:    time.Now(),
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
		ID:           phoneNumber,
		PhoneNumber:  phoneNumber,
		Connected:    false,
		IsPairing:    true,
		Client:       client,
		LastUsed:     time.Now(),
		UserID:       userID,
		CustomID:     customID,
		DailySent:    0,
		LastResetDay: time.Now().Day(),
		Trust:        40,
		CreatedAt:    time.Now(),
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
   MESSAGE HANDLING (ANTI-BAN QUEUE & SENDER)
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

// canSendNow returns whether it's okay to send right now (global daily cap, silent hours)
func (wm *WhatsAppManager) canSendNow() bool {
	now := time.Now()

	wm.mutex.Lock()
	// reset global daily counter at midnight (local server time)
	if now.Day() != wm.LastResetDay {
		wm.DailySent = 0
		wm.LastResetDay = now.Day()
		// slowly increase global trust score a bit each day
		if wm.TrustScore < 80 {
			wm.TrustScore += 5
		}
	}
	daily := wm.DailySent
	wm.mutex.Unlock()

	// silent hours check (handles wrap-around)
	if SilentStart <= SilentEnd {
		if now.Hour() >= SilentStart && now.Hour() <= SilentEnd {
			return false
		}
	} else {
		if now.Hour() >= SilentStart || now.Hour() <= SilentEnd {
			return false
		}
	}

	// global daily cap (use configured value)
	if daily >= DailyLimit {
		return false
	}

	return true
}

/* ---------------------------------------------------
   QUEUE + SENDER
--------------------------------------------------- */

func (wm *WhatsAppManager) processMessageQueue() {
	// seed rng for randomization (ensure this runs once)
	rand.Seed(time.Now().UnixNano())

	msgCounter := 0

	for msg := range wm.msgQueue {

		// Wait until both global & device safety allow sending
		for {
			// check global
			if !wm.canSendNow() {
				time.Sleep(15 * time.Second)
				continue
			}

			// check device existence & per-device safety
			wm.mutex.RLock()
			dev := wm.devices[msg.DeviceID]
			wm.mutex.RUnlock()

			if dev != nil && wm.deviceCanSend(dev) {
				break
			}

			// try to find any other device we can use
			found := false
			wm.mutex.RLock()
			for _, d := range wm.devices {
				if d.Connected && wm.deviceCanSend(d) {
					found = true
					break
				}
			}
			wm.mutex.RUnlock()
			if found {
				break
			}

			time.Sleep(15 * time.Second)
		}

		// Pre-send checks
		if msg.PhoneNumber == "" || strings.TrimSpace(msg.MessageText) == "" {
			wm.updateMsgStatus(msg.ID, "failed")
			continue
		}

		// Send and capture success
		success := wm.sendActualMessage(msg)

		// Update counters & trust score
		wm.mutex.Lock()
		if success {
			wm.DailySent++
			msgCounter++
			// small global trust bump occasionally
			if wm.TrustScore < 100 && rand.Intn(100) < 20 {
				wm.TrustScore++
			}
		} else {
			// penalize global trust on failures
			if wm.TrustScore > 10 {
				wm.TrustScore -= 8
			}
		}
		wm.mutex.Unlock()

		// 1) Base human delay between messages
		randomDelay(MinDelay, MaxDelay)

		// 2) If message contains link, add extra delay
		if strings.Contains(msg.MessageText, "http://") || strings.Contains(msg.MessageText, "https://") {
			randomDelay(LinkMinDelay, LinkMaxDelay)
		}

		// 3) Long break after N messages to simulate human pause
		if msgCounter%BreakEvery == 0 && msgCounter > 0 {
			randomDelay(BreakMin, BreakMax)
		}

		// 4) Occasional human idle (10-20% chance)
		if rand.Intn(100) < 15 {
			randomDelay(20, 60)
		}

		// 5) If global trust low, enforce a cooldown
		wm.mutex.RLock()
		ts := wm.TrustScore
		wm.mutex.RUnlock()
		if ts < 40 {
			randomDelay(30, 90)
		}
	}
}

func (wm *WhatsAppManager) sendActualMessage(message Message) bool {
	// find device requested
	wm.mutex.RLock()
	device, exists := wm.devices[message.DeviceID]
	wm.mutex.RUnlock()

	// fallback to any connected device if the requested one is offline or not allowed
	if !exists || !device.Connected || !wm.deviceCanSend(device) {
		// attempt to rotate to another good device
		wm.mutex.RLock()
		for _, d := range wm.devices {
			if d.Connected && wm.deviceCanSend(d) {
				device = d
				break
			}
		}
		wm.mutex.RUnlock()
	}

	if device == nil || !device.Connected {
		wm.updateMsgStatus(message.ID, "failed")
		return false
	}

	// check again deviceCanSend before actual send (race safe)
	if !wm.deviceCanSend(device) {
		wm.updateMsgStatus(message.ID, "failed")
		return false
	}

	jid, err := types.ParseJID(strings.TrimSpace(message.PhoneNumber) + "@s.whatsapp.net")
	if err != nil {
		wm.updateMsgStatus(message.ID, "failed")
		return false
	}

	finalText := humanize(message.MessageText)
	waMsg := &waE2E.Message{Conversation: proto.String(finalText)}

	// try send with one retry
	ctx, cancel := context.WithTimeout(wm.ctx, 30*time.Second)
	defer cancel()

	_, err = device.Client.SendMessage(ctx, jid, waMsg)
	if err != nil {
		// backoff and retry once
		randomDelay(5, 12)
		_, err2 := device.Client.SendMessage(wm.ctx, jid, waMsg)
		if err2 != nil {
			// failed after retry
			wm.updateMsgStatus(message.ID, "failed")
			wm.mutex.Lock()
			if device.Trust > 5 {
				device.Trust -= 5
			}
			wm.mutex.Unlock()
			log.Printf("Send failed to %s: %v / retry: %v\n", message.PhoneNumber, err, err2)
			return false
		}
	}

	// success: update device & DB
	wm.mutex.Lock()
	device.MessagesSent++
	device.LastUsed = time.Now()
	device.DailySent++
	if device.Trust < 100 {
		device.Trust += 2
	}
	wm.mutex.Unlock()

	wm.updateMsgStatus(message.ID, "sent")
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

	// push to the queue (will be sent by anti-ban queue)
	manager.msgQueue <- req

	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleSendBulk(w http.ResponseWriter, r *http.Request) {
	var req BulkRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", 400)
		return
	}

	// queue insertion is done in a goroutine so API returns quickly
	go func() {
		for _, phone := range req.PhoneNumbers {
			phone = strings.TrimSpace(phone)
			if phone == "" {
				continue
			}

			// Create message with unique ID
			msg := Message{
				ID:          fmt.Sprintf("bulk_%d_%s", time.Now().UnixNano(), phone),
				DeviceID:    req.DeviceID,
				PhoneNumber: phone,
				MessageText: req.Message,
				Status:      "pending",
				Timestamp:   time.Now(),
			}

			// insert to DB immediately
			_, _ = manager.db.Exec(`
				INSERT INTO messages (id, device_id, phone_number, message_text, status, timestamp)
				VALUES (?,?,?,?,?,?)
			`, msg.ID, msg.DeviceID, msg.PhoneNumber, msg.MessageText, msg.Status, msg.Timestamp)

			// push into queue — anti-ban queue will pace sends
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
            dev.Client.Disconnect()               // <-- FIXED
            _ = dev.Client.Logout(manager.ctx)    // <-- OK
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
   ANTI-BAN API ROUTES (for admin panel)
--------------------------------------------------- */

func handleAntiBanGet(w http.ResponseWriter, r *http.Request) {
	manager.mutex.RLock()
	defer manager.mutex.RUnlock()

	devices := []map[string]interface{}{}

	for _, d := range manager.devices {
		devices = append(devices, map[string]interface{}{
			"id":    d.ID,
			"phone": d.PhoneNumber,
			"sent":  d.DailySent,
			"limit": warmUpLimit(d),
			"trust": d.Trust,
		})
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"global":  AntiBanConfig,
		"devices": devices,
	})
}

func handleAntiBanSave(w http.ResponseWriter, r *http.Request) {
	var cfg AntiBanSettings
	_ = json.NewDecoder(r.Body).Decode(&cfg)

	AntiBanConfig = cfg
	_ = saveAntiBanConfig()

	// sync to globals if provided
	if cfg.DailyLimit != 0 {
		DailyLimit = cfg.DailyLimit
	}
	if cfg.MinDelay != 0 {
		MinDelay = cfg.MinDelay
	}
	if cfg.MaxDelay != 0 {
		MaxDelay = cfg.MaxDelay
	}
	if cfg.LinkMin != 0 {
		LinkMinDelay = cfg.LinkMin
	}
	if cfg.LinkMax != 0 {
		LinkMaxDelay = cfg.LinkMax
	}
	if cfg.BreakEvery != 0 {
		BreakEvery = cfg.BreakEvery
	}
	if cfg.BreakMin != 0 {
		BreakMin = cfg.BreakMin
	}
	if cfg.BreakMax != 0 {
		BreakMax = cfg.BreakMax
	}
	if cfg.SilentStart != 0 {
		SilentStart = cfg.SilentStart
	}
	if cfg.SilentEnd != 0 {
		SilentEnd = cfg.SilentEnd
	}

	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleAntiBanResetDevice(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["deviceId"]

	manager.mutex.Lock()
	if d, ok := manager.devices[id]; ok {
		d.DailySent = 0
		d.Trust = 60
		d.LastResetDay = time.Now().Day()
	}
	manager.mutex.Unlock()

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
	manager = NewWhatsAppManager()

	// Ensure rand seeded (NewWhatsAppManager also seeds, but double-safe)
	rand.Seed(time.Now().UnixNano())

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

	// ---------------------------
	// ADMIN AUTH ROUTES
	// ---------------------------
	api := r.PathPrefix("/api").Subrouter()
	api.HandleFunc("/admin-login", adminLogin).Methods("POST")
	api.HandleFunc("/admin-login-check", adminLoginCheck).Methods("GET")

	// ---------------------------
	// WHATSAPP API ROUTES
	// ---------------------------
	api.HandleFunc("/whatsapp/status", handleStatus).Methods("GET")
	api.HandleFunc("/whatsapp/send", handleSendSingle).Methods("POST")
	api.HandleFunc("/whatsapp/bulk", handleSendBulk).Methods("POST")
	api.HandleFunc("/messages", handleGetMessages).Methods("GET")

	earning := api.PathPrefix("/earning").Subrouter()
	earning.HandleFunc("/add-device", handleAddDevice).Methods("POST")
	earning.HandleFunc("/remove-device/{deviceId}", handleRemoveDevice).Methods("DELETE")

	// ANTI-BAN API ROUTES
	api.HandleFunc("/antiban/get", handleAntiBanGet).Methods("GET")
	api.HandleFunc("/antiban/save", handleAntiBanSave).Methods("POST")
	api.HandleFunc("/antiban/reset-device/{deviceId}", handleAntiBanResetDevice).Methods("POST")

	// ADMIN PAGE PROTECTION
	r.PathPrefix("/admin/").Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("adminSession")
		if err != nil || c.Value != "authenticated" {
			http.Redirect(w, r, "/login.html", http.StatusFound)
			return
		}
		http.FileServer(http.Dir("./public/")).ServeHTTP(w, r)
	}))

	// STATIC FILE SERVER
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./public/")))

	// START SERVER
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

	// Shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("Shutting down...")
	_ = manager.db.Close()
	_ = srv.Close()
}