package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
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
	QUEUE_PREFIX   = getEnv("QUEUE_PREFIX", "wuzapi_") // Configurable queue prefix
	ROUTING_PREFIX = getEnv("ROUTING_PREFIX", "wa")    // Configurable routing key prefix

	// Buffer Configuration (Disk-Based)
	BUFFER_DIR = getEnv("BUFFER_DIR", "./buffer") // Directory for failing webhooks

	// TTL Configuration - in minutes for environment, converted to milliseconds internally
	MESSAGE_TTL_MINUTES = getEnvInt("MESSAGE_TTL_MINUTES", 4320) // Default: 3 days = 4320 minutes
	MAX_QUEUE_LENGTH    = getEnvInt("MAX_QUEUE_LENGTH", 50000)   // Increased default for 3-day retention

	// Retry Configuration
	RETRY_ENABLED       = getEnvBool("RETRY_ENABLED", true)    // Enable/disable retry mechanism
	RETRY_DELAY         = getEnvInt("RETRY_DELAY", 60)         // Delay before retry in seconds (default: 60s)
	DLX_EXCHANGE_NAME   = getEnv("DLX_EXCHANGE_NAME", "")      // Dead Letter Exchange (auto-generated if empty)
	EXTRA_QUEUES_CONFIG = getEnv("EXTRA_QUEUES", "")           // Extra queues config (e.g. "molagis:2")

	// Reduced memory footprint configuration
	HEALTH_CHECK_INTERVAL = 15 * time.Second // Increased from 5s
	MAX_PAYLOAD_SIZE      = 64 * 1024        // 64KB limit per payload
	NTFY_URL              = getEnv("NTFY_URL", "https://ntfy.sh/monitor-server-30AhxaPwq00MzspW")
	NTFY_TIMEOUT          = 5 * time.Second // Reduced from 10s
)

// ===== KONFIGURASI SERVICES (MINIMAL) =====
var WEBHOOK_SERVICES = []string{
	"molagis",
	"muafa",
	"kurir",
}

// ===== STRUCTS (OPTIMIZED) =====
type WebhookService struct {
	conn                *amqp.Connection
	channel             *amqp.Channel
	isConnected         bool
	wasConnected        bool
	mutex               sync.RWMutex      // Protects connection state
	fileMutex           sync.Mutex        // Protects file operations
	lastNotifyTime      map[string]int64  // Use int64 instead of time.Time
	notifyMutex         sync.RWMutex
	consecutiveFailures uint8             // Use uint8 instead of int
}

type PayloadItem struct {
	RoutingKey string `json:"routing_key"`
	Data       []byte `json:"data"`
	Timestamp  int64  `json:"timestamp"`
}

// Minimal notification struct
type SimpleNotify struct {
	Title    string `json:"title,omitempty"`
	Message  string `json:"message"`
	Priority int    `json:"priority,omitempty"`
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

func getEnvBool(key string, fallback bool) bool {
	if value, exists := os.LookupEnv(key); exists {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return fallback
}

// Get DLX exchange name (auto-generate if not set)
func getDLXExchangeName() string {
	if DLX_EXCHANGE_NAME != "" {
		return DLX_EXCHANGE_NAME
	}
	return EXCHANGE_NAME + "_dlx"
}

// Parse extra queues config: "molagis:2,other:3" -> map["molagis"][]string{"2"}
func getExtraQueuesMap() map[string][]string {
	config := map[string][]string{}
	if EXTRA_QUEUES_CONFIG == "" {
		return config
	}

	pairs := strings.Split(EXTRA_QUEUES_CONFIG, ",")
	for _, p := range pairs {
		parts := strings.Split(p, ":")
		if len(parts) == 2 {
			service := strings.TrimSpace(parts[0])
			suffix := strings.TrimSpace(parts[1])
			config[service] = append(config[service], suffix)
		}
	}
	return config
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

// Get queue arguments with MESSAGE TTL and DLX (Dead Letter Exchange)
func getQueueArguments(dlxRoutingKey string) amqp.Table {
	args := amqp.Table{}

	// Convert minutes to milliseconds for RabbitMQ
	messageTTLMs := int32(MESSAGE_TTL_MINUTES * 60 * 1000)
	if messageTTLMs > 0 {
		args["x-message-ttl"] = messageTTLMs // Message TTL in milliseconds
	}

	if MAX_QUEUE_LENGTH > 0 {
		args["x-max-length"] = int32(MAX_QUEUE_LENGTH) // Max queue length
		args["x-overflow"] = "drop-head"               // Drop oldest messages when limit reached
	}

	// Add Dead Letter Exchange if retry is enabled
	if RETRY_ENABLED {
		args["x-dead-letter-exchange"] = getDLXExchangeName()
		// If explicit DLX routing key is provided, use it. Otherwise RabbitMQ uses original routing key.
		if dlxRoutingKey != "" {
			args["x-dead-letter-routing-key"] = dlxRoutingKey
		}
	}

	return args
}

// Get retry queue arguments - single retry queue with delay
// Messages wait here, then return to main queue for re-processing
func getRetryQueueArguments(delaySeconds int, returnRoutingKey string) amqp.Table {
	return amqp.Table{
		"x-message-ttl":             int32(delaySeconds * 1000), // Wait time before retry
		"x-dead-letter-exchange":    EXCHANGE_NAME,              // Route back to main exchange
		"x-dead-letter-routing-key": returnRoutingKey,           // Return to specific queue
		"x-max-length":              int32(1000),                // Limit retry queue size
		"x-overflow":                "reject-publish",           // Reject new messages when full (goes to DLQ)
	}
}

// Get DLQ (final dead letter queue) arguments
// Messages that can't be processed go here for manual review
func getDLQArguments() amqp.Table {
	return amqp.Table{
		"x-message-ttl": int32(MESSAGE_TTL_MINUTES * 60 * 1000), // Same TTL as main queue
	}
}

// Helper function to get message TTL in milliseconds
func getMessageTTLMs() int32 {
	return int32(MESSAGE_TTL_MINUTES * 60 * 1000)
}

func NewWebhookService() *WebhookService {
	// Create buffer directory if it doesn't exist
	if err := os.MkdirAll(BUFFER_DIR, 0755); err != nil {
		log.Fatalf("Failed to create buffer directory %s: %v", BUFFER_DIR, err)
	}

	service := &WebhookService{
		lastNotifyTime:      make(map[string]int64, 4), // Pre-allocate for 4 types
		wasConnected:        false,
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
			Message:  fmt.Sprintf("RabbitMQ connection lost at %s:%s. Switching to disk buffer.", RABBITMQ_HOST, RABBITMQ_PORT),
			Priority: 5,
		})
	}
}

func (ws *WebhookService) notifyRabbitMQUp(flushedCount int) {
	ws.sendNotification(SimpleNotify{
		Title:    "✅ RabbitMQ Restored",
		Message:  fmt.Sprintf("RabbitMQ restored. %d buffered files flushed from disk.", flushedCount),
		Priority: 3,
	})
}

func (ws *WebhookService) notifyServiceStarted() {
	ws.sendNotification(SimpleNotify{
		Title:    "🚀 Service Started",
		Message:  fmt.Sprintf("Webhook service started on port %s with prefix: %s. Disk buffer: %s", WEBHOOK_PORT, QUEUE_PREFIX, BUFFER_DIR),
		Priority: 2,
	})
}

// ===== RABBITMQ FUNCTIONS (WITH RETRY MECHANISM) =====
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

	// Declare main exchange
	err = ws.channel.ExchangeDeclare(EXCHANGE_NAME, "direct", true, false, false, false, nil)
	if err != nil {
		ws.channel.Close()
		ws.conn.Close()
		return err
	}

	// Declare DLX (Dead Letter Exchange) if retry is enabled
	if RETRY_ENABLED {
		dlxName := getDLXExchangeName()
		err = ws.channel.ExchangeDeclare(dlxName, "direct", true, false, false, false, nil)
		if err != nil {
			ws.channel.Close()
			ws.conn.Close()
			return fmt.Errorf("failed to declare DLX exchange %s: %v", dlxName, err)
		}
		log.Printf("Declared DLX exchange: %s", dlxName)
	}

	// Get extra queues configuration
	extraQueuesMap := getExtraQueuesMap()

	// Declare queues for each service
	for _, service := range WEBHOOK_SERVICES {
		_, routingKey, queueName := getWebhookConfig(service)

		// 1. Declare MAIN Service Queue
		// Unique return key for main queue: routingKey + ".main"
		// DLX routing key for main queue: routingKey + ".dlx.main"
		mainReturnKey := routingKey + ".main"
		mainDlxKey := routingKey + ".dlx.main"

		if err := ws.declareQueueSet(service, queueName, routingKey, mainReturnKey, mainDlxKey); err != nil {
			return err
		}

		// 2. Declare EXTRA Queues (if any)
		if suffixes, ok := extraQueuesMap[service]; ok {
			for _, suffix := range suffixes {
				extraQueueName := fmt.Sprintf("%s_%s", queueName, suffix)
				// Shared routing key remains the same (routingKey) -> fanout behavior
				// Unique keys for this specific queue:
				extraReturnKey := fmt.Sprintf("%s.%s", routingKey, suffix)
				extraDlxKey := fmt.Sprintf("%s.dlx.%s", routingKey, suffix)

				log.Printf("Setting up extra queue: %s (suffix: %s) for service: %s", extraQueueName, suffix, service)
				if err := ws.declareQueueSet(service, extraQueueName, routingKey, extraReturnKey, extraDlxKey); err != nil {
					return err
				}
			}
		}
	}

	ws.mutex.Lock()
	ws.isConnected = true
	ws.mutex.Unlock()

	log.Printf("Connected to RabbitMQ successfully at %s:%s", RABBITMQ_HOST, RABBITMQ_PORT)
	log.Printf("Using queue prefix: %s, routing prefix: %s", QUEUE_PREFIX, ROUTING_PREFIX)
	log.Printf("Message TTL: %d minutes (%d days), Max Queue Length: %d", MESSAGE_TTL_MINUTES, MESSAGE_TTL_MINUTES/1440, MAX_QUEUE_LENGTH)
	if RETRY_ENABLED {
		log.Printf("Retry enabled with %d second delay", RETRY_DELAY)
		log.Printf("DLX Exchange: %s", getDLXExchangeName())
		if EXTRA_QUEUES_CONFIG != "" {
			log.Printf("Extra queues configured: %s", EXTRA_QUEUES_CONFIG)
		}
	} else {
		log.Printf("Retry disabled")
	}
	return nil
}

// declareQueueSet declares a main queue, determines its DLX/Retry/DLQ setup, and binds it.
func (ws *WebhookService) declareQueueSet(serviceName, queueName, sharedRoutingKey, returnRoutingKey, dlxRoutingKey string) error {
	// Prepare queue arguments
	queueArgs := getQueueArguments(dlxRoutingKey)

	// Declare queue
	_, err := ws.channel.QueueDeclare(queueName, true, false, false, false, queueArgs)
	if err != nil {
		return fmt.Errorf("failed to declare queue %s: %v", queueName, err)
	}

	// 1. Bind to SHARED routing key (Primary input)
	err = ws.channel.QueueBind(queueName, sharedRoutingKey, EXCHANGE_NAME, false, nil)
	if err != nil {
		return fmt.Errorf("failed to bind queue %s to shared key %s: %v", queueName, sharedRoutingKey, err)
	}

	// 2. Bind to UNIQUE RETURN key (Retry return input)
	err = ws.channel.QueueBind(queueName, returnRoutingKey, EXCHANGE_NAME, false, nil)
	if err != nil {
		return fmt.Errorf("failed to bind queue %s to return key %s: %v", queueName, returnRoutingKey, err)
	}

	if RETRY_ENABLED {
		dlxName := getDLXExchangeName()

		// Declare Retry Queue
		retryQueueName := fmt.Sprintf("%s_retry", queueName)
		retryDelay := RETRY_DELAY
		// Retry queue returns to the UNIQUE RETURN KEY
		retryArgs := getRetryQueueArguments(retryDelay, returnRoutingKey)

		_, err = ws.channel.QueueDeclare(retryQueueName, true, false, false, false, retryArgs)
		if err != nil {
			return fmt.Errorf("failed to declare retry queue %s: %v", retryQueueName, err)
		}

		// Bind Retry Queue to DLX using the DLX Routing Key
		// This receives the REJECTED messages from the Main Queue
		err = ws.channel.QueueBind(retryQueueName, dlxRoutingKey, dlxName, false, nil)
		if err != nil {
			return fmt.Errorf("failed to bind retry queue %s: %v", retryQueueName, err)
		}

		// Declare DLQ (Dead Letter Queue) - for overflow
		dlqName := fmt.Sprintf("%s_dlq", queueName)
		dlqArgs := getDLQArguments()

		_, err = ws.channel.QueueDeclare(dlqName, true, false, false, false, dlqArgs)
		if err != nil {
			return fmt.Errorf("failed to declare DLQ %s: %v", dlqName, err)
		}

		log.Printf("Declared queue: %s [Bind: %s, %s] -> DLX Key: %s -> Retry: %s -> Return Key: %s",
			queueName, sharedRoutingKey, returnRoutingKey, dlxRoutingKey, retryQueueName, returnRoutingKey)
	}

	return nil
}

func (ws *WebhookService) disconnect() {
	ws.mutex.Lock()
	defer ws.mutex.Unlock()

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
		ws.mutex.RLock()
		connected := ws.isConnected
		ws.mutex.RUnlock()

		if !connected {
			if err := ws.connectRabbitMQ(); err != nil {
				log.Printf("Failed to connect to RabbitMQ: %v", err)
				ws.consecutiveFailures++
				if ws.consecutiveFailures >= 3 && ws.wasConnected {
					ws.notifyRabbitMQDown()
					ws.wasConnected = false
				}
			} else {
				if !ws.wasConnected && ws.consecutiveFailures > 0 {
					// Flush payloads from disk
					count := ws.flushBuffer()
					ws.notifyRabbitMQUp(count)
				}
				ws.wasConnected = true
				ws.consecutiveFailures = 0
			}
		} else {
			if ws.conn != nil && ws.conn.IsClosed() {
				ws.mutex.Lock()
				ws.isConnected = false
				ws.mutex.Unlock()
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

	ws.mutex.RLock()
	connected := ws.isConnected
	ws.mutex.RUnlock()

	if !connected {
		ws.addToBuffer(routingKey, data)
		return fmt.Errorf("RabbitMQ down, payload saved to disk")
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
		ws.mutex.Lock()
		ws.isConnected = false
		ws.mutex.Unlock()
		ws.addToBuffer(routingKey, data)
		return err
	}

	return nil
}

// ===== DISK BUFFER FUNCTIONS =====

func (ws *WebhookService) addToBuffer(routingKey string, data []byte) {
	ws.fileMutex.Lock()
	defer ws.fileMutex.Unlock()

	generateID := uuid.New().String()
	timestamp := time.Now().UnixNano()

	payload := PayloadItem{
		RoutingKey: routingKey,
		Data:       data,
		Timestamp:  timestamp,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal payload for backup: %v", err)
		return
	}

	// Filename: timestamp_routingKey_uuid.json to ensure uniqueness and sortability
	filename := fmt.Sprintf("%d_%s_%s.json", timestamp, routingKey, generateID)
	filepath := filepath.Join(BUFFER_DIR, filename)

	if err := os.WriteFile(filepath, jsonData, 0644); err != nil {
		log.Printf("CRITICAL: Failed to write payload to disk: %v", err)
	} else {
		log.Printf("Payload buffered to disk: %s", filename)
	}
}

func (ws *WebhookService) flushBuffer() int {
	ws.fileMutex.Lock()
	defer ws.fileMutex.Unlock()

	files, err := ioutil.ReadDir(BUFFER_DIR)
	if err != nil {
		log.Printf("Failed to read buffer directory: %v", err)
		return 0
	}

	count := 0
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		fullPath := filepath.Join(BUFFER_DIR, file.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			log.Printf("Failed to read buffered file %s: %v", file.Name(), err)
			continue
		}

		var payload PayloadItem
		if err := json.Unmarshal(data, &payload); err != nil {
			log.Printf("Failed to unmarshal buffered file %s: %v", file.Name(), err)
			// Move corrupt file or rename? For now, keep it to avoid data loss until manual inspection
			continue 
		}

		// Try to publish
		// Note: We use channel directly here to avoid circular logic with publishMessage logic
		publishing := amqp.Publishing{
			ContentType:  "application/json",
			Body:         payload.Data,
			DeliveryMode: amqp.Persistent,
		}

		messageTTLMs := getMessageTTLMs()
		if messageTTLMs > 0 {
			publishing.Expiration = fmt.Sprintf("%d", messageTTLMs)
		}

		err = ws.channel.Publish(EXCHANGE_NAME, payload.RoutingKey, false, false, publishing)

		if err != nil {
			log.Printf("Failed to flush payload %s: %v. Retaining file.", file.Name(), err)
			ws.mutex.Lock()
			ws.isConnected = false
			ws.mutex.Unlock()
			return count // Stop flushing if connection fails
		}

		// Success: Delete file
		if err := os.Remove(fullPath); err != nil {
			log.Printf("Failed to delete flushed file %s: %v", file.Name(), err)
		} else {
			count++
		}
	}
	return count
}

func (ws *WebhookService) getBufferedFileCount() int {
	ws.fileMutex.Lock()
	defer ws.fileMutex.Unlock()
	
	files, err := ioutil.ReadDir(BUFFER_DIR)
	if err != nil {
		return 0
	}
	count := 0
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".json") {
			count++
		}
	}
	return count
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
	
	filesCount := ws.getBufferedFileCount()

	status := map[string]interface{}{
		"status":    "healthy",
		"rabbitmq":  ws.isConnected,
		"queued":    filesCount, // Now backed by disk files
		"failures":  ws.consecutiveFailures,
		"timestamp": time.Now().Unix(),
		"configuration": map[string]interface{}{
			"queue_prefix":        QUEUE_PREFIX,
			"routing_prefix":      ROUTING_PREFIX,
			"exchange_name":       EXCHANGE_NAME,
			"max_payload_size":    MAX_PAYLOAD_SIZE,
			"message_ttl_minutes": MESSAGE_TTL_MINUTES,
			"message_ttl_days":    float64(MESSAGE_TTL_MINUTES) / 1440.0,
			"message_ttl_ms":      getMessageTTLMs(),
			"max_queue_length":    MAX_QUEUE_LENGTH,
			"buffer_dir":          BUFFER_DIR,
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
	log.Printf("Webhook server starting on port %s (Disk Buffer Secured)", WEBHOOK_PORT)
	log.Printf("RabbitMQ: %s:%s", RABBITMQ_HOST, RABBITMQ_PORT)
	log.Printf("Exchange: %s", EXCHANGE_NAME)
	log.Printf("Queue prefix: %s, Routing prefix: %s", QUEUE_PREFIX, ROUTING_PREFIX)
	log.Printf("Buffer Dir: %s", BUFFER_DIR)
	log.Printf("Message TTL: %d minutes (%.1f days), Queues: persistent", MESSAGE_TTL_MINUTES, float64(MESSAGE_TTL_MINUTES)/1440.0)

	log.Fatal(http.ListenAndServe(listenAddr, r))
}