package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"           
    "log"
    "net/http"
    "os"
    "strconv"
    "sync"
    "time"

    "github.com/gorilla/mux"
    "github.com/streadway/amqp"
)

// ===== KONFIGURASI TERPUSAT =====
var (
	// Server Configuration
	WEBHOOK_PORT = getEnv("WEBHOOK_PORT", "8001")

	// RabbitMQ Configuration
	RABBITMQ_HOST     = getEnv("RABBITMQ_HOST", "localhost")
	RABBITMQ_PORT     = getEnv("RABBITMQ_PORT", "5672")
	RABBITMQ_USER     = getEnv("RABBITMQ_USER", "wuzapi")
	RABBITMQ_PASSWORD = getEnv("RABBITMQ_PASSWORD", "mantab")
	RABBITMQ_VHOST    = getEnv("RABBITMQ_VHOST", "/")
	EXCHANGE_NAME     = getEnv("EXCHANGE_NAME", "wuzapi")

	// Queue Configuration
	QUEUE_PREFIX      = getEnv("QUEUE_PREFIX", "wuzapi_")        // NEW: Configurable queue prefix
	ROUTING_PREFIX    = getEnv("ROUTING_PREFIX", "wa")          // NEW: Configurable routing key prefix
	
	// TTL Configuration (NEW) - in minutes for environment, converted to milliseconds internally
	MESSAGE_TTL_MINUTES = getEnvInt("MESSAGE_TTL_MINUTES", 4320)  // Default: 3 days = 4320 minutes
	MAX_QUEUE_LENGTH    = getEnvInt("MAX_QUEUE_LENGTH", 50000)    // Increased default for 3-day retention

	// Reduced memory footprint configuration
	HEALTH_CHECK_INTERVAL = 15 * time.Second  // Increased from 5s
	MAX_PAYLOADS         = 1000               // Increased buffer for RabbitMQ downtime
	MAX_PAYLOAD_SIZE     = 64 * 1024          // 64KB limit per payload
	NTFY_URL            = getEnv("NTFY_URL", "https://ntfy.sh/monitor-server-30AhxaPwq00MzspW")
	NTFY_TIMEOUT        = 5 * time.Second     // Reduced from 10s
)

// ===== KONFIGURASI SERVICES (MINIMAL) =====
var WEBHOOK_SERVICES = []string{
	"molagis",
	"muafa", 
	"kurir",
}

// ===== STRUCTS (OPTIMIZED) =====
type WebhookService struct {
	conn               *amqp.Connection
	channel            *amqp.Channel
	isConnected        bool
	wasConnected       bool
	payloadQueue       []PayloadItem
	mutex              sync.RWMutex
	lastNotifyTime     map[string]int64  // Use int64 instead of time.Time
	notifyMutex        sync.RWMutex
	consecutiveFailures uint8            // Use uint8 instead of int
}

type PayloadItem struct {
	RoutingKey string
	Data       []byte
	Timestamp  int64     // Use int64 instead of time.Time
}

// Minimal notification struct
type SimpleNotify struct {
	Title   string `json:"title,omitempty"`
	Message string `json:"message"`
	Priority int   `json:"priority,omitempty"`
}

// ===== HELPER FUNCTIONS =====
func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if value, exists := os.LookupEnv(key); exists {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return fallback
}

func getRabbitMQURL() string {
	return fmt.Sprintf("amqp://%s:%s@%s:%s%s",
		RABBITMQ_USER, RABBITMQ_PASSWORD, RABBITMQ_HOST, RABBITMQ_PORT, RABBITMQ_VHOST)
}

// Optimized route generation with configurable prefixes
func getWebhookConfig(service string) (string, string, string) {
	return fmt.Sprintf("/webhook/%s", service),
		   fmt.Sprintf("%s.%s", ROUTING_PREFIX, service),
		   fmt.Sprintf("%s%s", QUEUE_PREFIX, service)
}

// NEW: Get queue arguments with MESSAGE TTL only (no queue auto-delete)
func getQueueArguments() amqp.Table {
	args := amqp.Table{}
	
	// Convert minutes to milliseconds for RabbitMQ
	messageTTLMs := int32(MESSAGE_TTL_MINUTES * 60 * 1000)
	if messageTTLMs > 0 {
		args["x-message-ttl"] = messageTTLMs // Message TTL in milliseconds
	}
	
	if MAX_QUEUE_LENGTH > 0 {
		args["x-max-length"] = int32(MAX_QUEUE_LENGTH) // Max queue length
		args["x-overflow"] = "drop-head" // Drop oldest messages when limit reached
	}
	
	// NO x-expires (queue TTL) - queue will persist
	
	return args
}

// Helper function to get message TTL in milliseconds
func getMessageTTLMs() int32 {
	return int32(MESSAGE_TTL_MINUTES * 60 * 1000)
}

func NewWebhookService() *WebhookService {
	service := &WebhookService{
		payloadQueue:       make([]PayloadItem, 0, MAX_PAYLOADS), // Pre-allocate capacity
		lastNotifyTime:     make(map[string]int64, 4),            // Pre-allocate for 4 types
		wasConnected:       false,
		consecutiveFailures: 0,
	}
	
	go service.healthMonitor()
	return service
}

// ===== NOTIFICATION FUNCTIONS (SIMPLIFIED) =====
func (ws *WebhookService) sendNotification(notify SimpleNotify) {
	go func() {
		// Minimal notification implementation
		body := bytes.NewBufferString(notify.Message)
		client := &http.Client{Timeout: NTFY_TIMEOUT}
		req, err := http.NewRequest("POST", NTFY_URL, body)
		if err != nil {
			return // Silent fail to save memory
		}

		if notify.Title != "" {
			req.Header.Set("Title", notify.Title)
		}
		if notify.Priority != 0 {
			req.Header.Set("Priority", fmt.Sprintf("%d", notify.Priority))
		}
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")

		resp, err := client.Do(req)
		if err != nil {
			return // Silent fail
		}
		resp.Body.Close() // Immediately close to free memory
	}()
}

func (ws *WebhookService) shouldNotify(eventType string, cooldownMinutes int) bool {
	ws.notifyMutex.Lock()
	defer ws.notifyMutex.Unlock()
	
	now := time.Now().Unix()
	lastTime, exists := ws.lastNotifyTime[eventType]
	if !exists || (now-lastTime) > int64(cooldownMinutes*60) {
		ws.lastNotifyTime[eventType] = now
		return true
	}
	return false
}

func (ws *WebhookService) notifyRabbitMQDown() {
	if ws.shouldNotify("rabbitmq_down", 20) { // 20 minutes cooldown
		ws.sendNotification(SimpleNotify{
			Title:    "🚨 RabbitMQ Down",
			Message:  fmt.Sprintf("RabbitMQ connection lost at %s:%s", RABBITMQ_HOST, RABBITMQ_PORT),
			Priority: 5,
		})
	}
}

func (ws *WebhookService) notifyRabbitMQUp(queuedCount int) {
	ws.sendNotification(SimpleNotify{
		Title:    "✅ RabbitMQ Restored",
		Message:  fmt.Sprintf("RabbitMQ restored. %d payloads flushed.", queuedCount),
		Priority: 3,
	})
}

func (ws *WebhookService) notifyPayloadQueueFull() {
	if ws.shouldNotify("queue_full", 60) { // 60 minutes cooldown
		ws.sendNotification(SimpleNotify{
			Title:    "⚠️ Queue Full",
			Message:  fmt.Sprintf("Payload queue full (%d). Dropping old payloads.", MAX_PAYLOADS),
			Priority: 4,
		})
	}
}

func (ws *WebhookService) notifyServiceStarted() {
	ws.sendNotification(SimpleNotify{
		Title:    "🚀 Service Started",
		Message:  fmt.Sprintf("Webhook service started on port %s with prefix: %s", WEBHOOK_PORT, QUEUE_PREFIX),
		Priority: 2,
	})
}

// ===== RABBITMQ FUNCTIONS (OPTIMIZED WITH MESSAGE TTL ONLY) =====
func (ws *WebhookService) connectRabbitMQ() error {
	var err error
	
	// Connect to RabbitMQ
	ws.conn, err = amqp.Dial(getRabbitMQURL())
	if err != nil {
		return err
	}

	// Create channel
	ws.channel, err = ws.conn.Channel()
	if err != nil {
		ws.conn.Close()
		return err
	}

	// Declare exchange
	err = ws.channel.ExchangeDeclare(EXCHANGE_NAME, "direct", true, false, false, false, nil)
	if err != nil {
		ws.channel.Close()
		ws.conn.Close()
		return err
	}

	// Get queue arguments with MESSAGE TTL only
	queueArgs := getQueueArguments()

	// Declare queues for each service with message TTL arguments (persistent queues)
	for _, service := range WEBHOOK_SERVICES {
		_, routingKey, queueName := getWebhookConfig(service)
		
		// Declare queue with message TTL arguments - durable=true, autoDelete=false
		_, err = ws.channel.QueueDeclare(queueName, true, false, false, false, queueArgs)
		if err != nil {
			ws.channel.Close()
			ws.conn.Close()
			return fmt.Errorf("failed to declare queue %s: %v", queueName, err)
		}

		// Bind queue
		err = ws.channel.QueueBind(queueName, routingKey, EXCHANGE_NAME, false, nil)
		if err != nil {
			ws.channel.Close()
			ws.conn.Close()
			return fmt.Errorf("failed to bind queue %s to routing key %s: %v", queueName, routingKey, err)
		}
	}

	ws.isConnected = true
	log.Printf("Connected to RabbitMQ successfully at %s:%s", RABBITMQ_HOST, RABBITMQ_PORT)
	log.Printf("Using queue prefix: %s, routing prefix: %s", QUEUE_PREFIX, ROUTING_PREFIX)
	log.Printf("Message TTL: %d minutes (%d days), Max Queue Length: %d", MESSAGE_TTL_MINUTES, MESSAGE_TTL_MINUTES/1440, MAX_QUEUE_LENGTH)
	log.Printf("Queues are persistent (no auto-delete)")
	return nil
}

func (ws *WebhookService) disconnect() {
	if ws.channel != nil {
		ws.channel.Close()
		ws.channel = nil
	}
	if ws.conn != nil {
		ws.conn.Close()
		ws.conn = nil
	}
	ws.isConnected = false
}

func (ws *WebhookService) healthMonitor() {
	for {
		if !ws.isConnected {
			if err := ws.connectRabbitMQ(); err != nil {
				ws.consecutiveFailures++
				if ws.consecutiveFailures >= 3 && ws.wasConnected {
					ws.notifyRabbitMQDown()
					ws.wasConnected = false
				}
			} else {
				if !ws.wasConnected && ws.consecutiveFailures > 0 {
					queuedCount := len(ws.payloadQueue)
					ws.flushPayloads()
					ws.notifyRabbitMQUp(queuedCount)
				}
				ws.wasConnected = true
				ws.consecutiveFailures = 0
			}
		} else {
			if ws.conn != nil && ws.conn.IsClosed() {
				ws.isConnected = false
			}
		}
		time.Sleep(HEALTH_CHECK_INTERVAL)
	}
}

func (ws *WebhookService) publishMessage(routingKey string, data []byte) error {
	// Check payload size limit
	if len(data) > MAX_PAYLOAD_SIZE {
		return fmt.Errorf("payload too large: %d bytes (max: %d)", len(data), MAX_PAYLOAD_SIZE)
	}

	ws.mutex.Lock()
	defer ws.mutex.Unlock()

	if !ws.isConnected {
		ws.addToPayloadQueue(routingKey, data)
		return fmt.Errorf("RabbitMQ down, payload queued")
	}

	// Create publishing with message TTL
	publishing := amqp.Publishing{
		ContentType:  "application/json",
		Body:         data,
		DeliveryMode: amqp.Persistent,
	}

	// Add message TTL (individual message TTL in milliseconds as string)
	messageTTLMs := getMessageTTLMs()
	if messageTTLMs > 0 {
		publishing.Expiration = fmt.Sprintf("%d", messageTTLMs)
	}

	err := ws.channel.Publish(EXCHANGE_NAME, routingKey, false, false, publishing)

	if err != nil {
		ws.isConnected = false
		ws.addToPayloadQueue(routingKey, data)
		return err
	}

	return nil
}

func (ws *WebhookService) addToPayloadQueue(routingKey string, data []byte) {
	now := time.Now().Unix()
	
	// Create copy of data to avoid reference issues
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)
	
	payload := PayloadItem{
		RoutingKey: routingKey,
		Data:       dataCopy,
		Timestamp:  now,
	}

	// Add to queue
	ws.payloadQueue = append(ws.payloadQueue, payload)

	// Remove old payloads if exceeded max
	if len(ws.payloadQueue) > MAX_PAYLOADS {
		// Remove from front (FIFO)
		copy(ws.payloadQueue, ws.payloadQueue[1:])
		ws.payloadQueue = ws.payloadQueue[:MAX_PAYLOADS]
		ws.notifyPayloadQueueFull()
	}
}

func (ws *WebhookService) flushPayloads() {
	ws.mutex.Lock()
	defer ws.mutex.Unlock()

	if len(ws.payloadQueue) == 0 {
		return
	}

	for _, payload := range ws.payloadQueue {
		// Create publishing with message TTL
		publishing := amqp.Publishing{
			ContentType:  "application/json",
			Body:         payload.Data,
			DeliveryMode: amqp.Persistent,
		}

		// Add message TTL
		messageTTLMs := getMessageTTLMs()
		if messageTTLMs > 0 {
			publishing.Expiration = fmt.Sprintf("%d", messageTTLMs)
		}

		err := ws.channel.Publish(EXCHANGE_NAME, payload.RoutingKey, false, false, publishing)

		if err != nil {
			ws.isConnected = false
			return
		}
	}

	// Clear the queue by resetting slice
	ws.payloadQueue = ws.payloadQueue[:0]
}

// ===== WEBHOOK HANDLERS (OPTIMIZED) =====
func (ws *WebhookService) handleWebhook(w http.ResponseWriter, r *http.Request, routingKey, webhookType string) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    // Limit request body size
    r.Body = http.MaxBytesReader(w, r.Body, int64(MAX_PAYLOAD_SIZE))
    
    // Read body with size limit
    data, err := io.ReadAll(r.Body)
    if err != nil {
        http.Error(w, "Failed to read request body", http.StatusBadRequest)
        return
    }
    r.Body.Close()

    if len(data) == 0 {
        http.Error(w, "Empty request body", http.StatusBadRequest)
        return
    }

    // Minimal JSON validation
    if data[0] != '{' && data[0] != '[' {
        http.Error(w, "Invalid JSON format", http.StatusBadRequest)
        return
    }

    // Publish message
    err = ws.publishMessage(routingKey, data)
    if err != nil {
        log.Printf("Webhook %s publishing error: %v", webhookType, err)
    }

    // Minimal success response
    w.Header().Set("Content-Type", "application/json")
    w.Write([]byte(`{"status":"success"}`))
}

// Health check endpoint with TTL info in minutes and days
func (ws *WebhookService) healthCheck(w http.ResponseWriter, r *http.Request) {
	// Generate route info for health check
	routes := make(map[string]map[string]string)
	for _, service := range WEBHOOK_SERVICES {
		path, routingKey, queueName := getWebhookConfig(service)
		routes[service] = map[string]string{
			"path":        path,
			"routing_key": routingKey,
			"queue_name":  queueName,
		}
	}

	status := map[string]interface{}{
		"status":          "healthy",
		"rabbitmq":        ws.isConnected,
		"queued":          len(ws.payloadQueue),
		"failures":        ws.consecutiveFailures,
		"timestamp":       time.Now().Unix(),
		"configuration": map[string]interface{}{
			"queue_prefix":       QUEUE_PREFIX,
			"routing_prefix":     ROUTING_PREFIX,
			"exchange_name":      EXCHANGE_NAME,
			"max_payloads":       MAX_PAYLOADS,
			"max_payload_size":   MAX_PAYLOAD_SIZE,
			"message_ttl_minutes": MESSAGE_TTL_MINUTES,
			"message_ttl_days":   float64(MESSAGE_TTL_MINUTES) / 1440.0,
			"message_ttl_ms":     getMessageTTLMs(),
			"max_queue_length":   MAX_QUEUE_LENGTH,
			"queue_auto_delete":  false,
		},
		"routes": routes,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// Test notification (minimal)
func (ws *WebhookService) testNotification(w http.ResponseWriter, r *http.Request) {
	ws.sendNotification(SimpleNotify{
		Title:    "🧪 Test",
		Message:  fmt.Sprintf("Test notification from %s service", QUEUE_PREFIX),
		Priority: 2,
	})
	
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"success"}`))
}

func main() {
	// Initialize webhook service
	webhookService := NewWebhookService()
	defer webhookService.disconnect()

	// Send startup notification
	webhookService.notifyServiceStarted()

	// Setup routes
	r := mux.NewRouter()
	
	// Register webhook endpoints
	for _, service := range WEBHOOK_SERVICES {
		path, routingKey, queueName := getWebhookConfig(service)
		
		// Capture variables for closure
		rKey := routingKey
		svcName := service
		
		r.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			webhookService.handleWebhook(w, r, rKey, svcName)
		}).Methods("POST")
		
		log.Printf("Registered webhook: %s -> %s -> %s", path, routingKey, queueName)
	}
	
	// Utility endpoints
	r.HandleFunc("/health", webhookService.healthCheck).Methods("GET")
	r.HandleFunc("/test-notification", webhookService.testNotification).Methods("GET")

	// Start server
	listenAddr := fmt.Sprintf(":%s", WEBHOOK_PORT)
	log.Printf("Webhook server starting on port %s (Memory optimized)", WEBHOOK_PORT)
	log.Printf("RabbitMQ: %s:%s", RABBITMQ_HOST, RABBITMQ_PORT)
	log.Printf("Exchange: %s", EXCHANGE_NAME)
	log.Printf("Queue prefix: %s, Routing prefix: %s", QUEUE_PREFIX, ROUTING_PREFIX)
	log.Printf("Max payloads: %d, Max size: %dKB", MAX_PAYLOADS, MAX_PAYLOAD_SIZE/1024)
	log.Printf("Message TTL: %d minutes (%.1f days), Queues: persistent", MESSAGE_TTL_MINUTES, float64(MESSAGE_TTL_MINUTES)/1440.0)
	
	log.Fatal(http.ListenAndServe(listenAddr, r))
}