package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ===== DATA TYPES =====

type Source struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`      // e.g. "/webhook/myapp"
	AuthToken string `json:"auth_token"` // optional bearer token
	Routes    []Route `json:"routes"`
	CreatedAt int64  `json:"created_at"`
}

type Route struct {
	ID           string `json:"id"`
	SourceID     string `json:"source_id"`
	Exchange     string `json:"exchange"`
	RoutingKey   string `json:"routing_key"`
	QueuePrefix  string `json:"queue_prefix"`
	DeviceFilter string `json:"device_filter"` // "*" = all, "abc" = exact, "s23*" = prefix
	FilterField  string `json:"filter_field"`  // JSON field to match (e.g. "device_id")
	Enabled      bool   `json:"enabled"`
	Priority     int    `json:"priority"`
	CreatedAt    int64  `json:"created_at"`
}

type RMQConfig struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	VHost    string `json:"vhost"`
	Exchange string `json:"exchange"`
}

type EnvParams struct {
	WebhookPort     string `json:"webhook_port,omitempty"`
	QueuePrefix     string `json:"queue_prefix,omitempty"`
	RoutingPrefix   string `json:"routing_prefix,omitempty"`
	ExchangeName    string `json:"exchange_name,omitempty"`
	MessageTTLMin   *int   `json:"message_ttl_minutes,omitempty"`
	MaxQueueLength  *int   `json:"max_queue_length,omitempty"`
	RetryEnabled    *bool  `json:"retry_enabled,omitempty"`
	RetryDelay      *int   `json:"retry_delay,omitempty"`
	DLXExchangeName string `json:"dlx_exchange_name,omitempty"`
	BufferDir       string `json:"buffer_dir,omitempty"`
	MaxPayloadSize  *int   `json:"max_payload_size,omitempty"`
	NtfyURL         string `json:"ntfy_url,omitempty"`
	RMQMgmtURL      string `json:"rmq_mgmt_url,omitempty"`
	RMQMgmtUser     string `json:"rmq_mgmt_user,omitempty"`
	RMQMgmtPassword string `json:"rmq_mgmt_password,omitempty"`
	SaveDir         string `json:"save_dir,omitempty"`
}

type Config struct {
	Version int       `json:"version"`
	RMQ     RMQConfig `json:"rabbitmq"`
	Env     EnvParams `json:"env,omitempty"`
	Sources []Source  `json:"sources"`
}

// ===== CONFIG MANAGER =====

type ConfigManager struct {
	mu     sync.RWMutex
	config Config
	path   string
}

var SAVE_DIR = getEnv("SAVE_DIR", ".")

func NewConfigManager() *ConfigManager {
	cm := &ConfigManager{
		config: Config{Version: 1},
		path:   filepath.Join(SAVE_DIR, "config.json"),
	}
	cm.load()
	return cm
}

func (cm *ConfigManager) load() {
	data, err := os.ReadFile(cm.path)
	if err != nil {
		log.Printf("No existing config, starting fresh (%v)", err)
		cm.config = Config{
			Version: 1,
			Sources: make([]Source, 0),
		}
		return
	}
	if err := json.Unmarshal(data, &cm.config); err != nil {
		log.Printf("Config corrupt, starting fresh: %v", err)
		cm.config = Config{Version: 1, Sources: make([]Source, 0)}
		return
	}
	applyRMQToGlobals(cm.config.RMQ)
	log.Printf("Loaded config with %d sources", len(cm.config.Sources))
}

func (cm *ConfigManager) save() error {
	data, err := json.MarshalIndent(cm.config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := cm.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, cm.path); err != nil {
		return fmt.Errorf("atomic rename: %w", err)
	}
	return nil
}

// READ

func (cm *ConfigManager) GetConfig() Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.config
}

func (cm *ConfigManager) GetSourceByPath(path string) *Source {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for i := range cm.config.Sources {
		if cm.config.Sources[i].Path == path {
			return &cm.config.Sources[i]
		}
	}
	return nil
}

func (cm *ConfigManager) GetSourceByID(id string) *Source {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for i := range cm.config.Sources {
		if cm.config.Sources[i].ID == id {
			return &cm.config.Sources[i]
		}
	}
	return nil
}

// WRITE

func (cm *ConfigManager) UpsertSource(name, path, authToken string) (Source, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for i := range cm.config.Sources {
		if cm.config.Sources[i].Name == name {
			cm.config.Sources[i].Path = path
			cm.config.Sources[i].AuthToken = authToken
			if err := cm.save(); err != nil {
				return Source{}, err
			}
			return cm.config.Sources[i], nil
		}
	}

	s := Source{
		ID:        uuid.New().String()[:8],
		Name:      name,
		Path:      path,
		AuthToken: authToken,
		Routes:    make([]Route, 0),
		CreatedAt: time.Now().Unix(),
	}
	cm.config.Sources = append(cm.config.Sources, s)
	if err := cm.save(); err != nil {
		return Source{}, err
	}
	return s, nil
}

func (cm *ConfigManager) DeleteSource(id string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for i := range cm.config.Sources {
		if cm.config.Sources[i].ID == id {
			cm.config.Sources = append(cm.config.Sources[:i], cm.config.Sources[i+1:]...)
			return cm.save()
		}
	}
	return fmt.Errorf("source %s not found", id)
}

func (cm *ConfigManager) GetRMQ() RMQConfig {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.config.RMQ
}

func applyRMQToGlobals(rmq RMQConfig) {
	if rmq.Host != "" { RABBITMQ_HOST = rmq.Host }
	if rmq.Port != "" { RABBITMQ_PORT = rmq.Port }
	if rmq.User != "" { RABBITMQ_USER = rmq.User }
	if rmq.Password != "" { RABBITMQ_PASSWORD = rmq.Password }
	if rmq.VHost != "" { RABBITMQ_VHOST = rmq.VHost }
	if rmq.Exchange != "" { EXCHANGE_NAME = rmq.Exchange }
}

func (cm *ConfigManager) UpdateRMQ(rmq RMQConfig) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.config.RMQ = rmq
	applyRMQToGlobals(rmq)
	if err := cm.save(); err != nil {
		return err
	}
	log.Printf("RMQ config updated: %s:%s exchange=%s", rmq.Host, rmq.Port, rmq.Exchange)
	return nil
}

// BuildEnvConfig returns the effective config (env default + saved override)
func BuildEnvConfig(cfg EnvParams) map[string]interface{} {
	m := map[string]interface{}{
		"webhook_port":      getEnvWithOverride("WEBHOOK_PORT", "8001", cfg.WebhookPort),
		"queue_prefix":      getEnvWithOverride("QUEUE_PREFIX", "wuzapi_", cfg.QueuePrefix),
		"routing_prefix":    getEnvWithOverride("ROUTING_PREFIX", "wa", cfg.RoutingPrefix),
		"exchange_name":     getEnvWithOverride("EXCHANGE_NAME", "wuzapi", cfg.ExchangeName),
		"buffer_dir":        getEnvWithOverride("BUFFER_DIR", "./buffer", cfg.BufferDir),
		"dlx_exchange_name": getEnvWithOverride("DLX_EXCHANGE_NAME", "", cfg.DLXExchangeName),
		"ntfy_url":          getEnvWithOverride("NTFY_URL", "https://ntfy.sh/monitor-server-30AhxaPwq00MzspW", cfg.NtfyURL),
		"rmq_mgmt_url":      getEnvWithOverride("RMQ_MGMT_URL", "http://localhost:15672", cfg.RMQMgmtURL),
		"rmq_mgmt_user":     getEnvWithOverride("RMQ_MGMT_USER", "guest", cfg.RMQMgmtUser),
		"rmq_mgmt_password": getEnvWithOverride("RMQ_MGMT_PASSWORD", "guest", cfg.RMQMgmtPassword),
		"save_dir":          getEnvWithOverride("SAVE_DIR", ".", cfg.SaveDir),
		"message_ttl_minutes": intPtrOrDefault(cfg.MessageTTLMin, MESSAGE_TTL_MINUTES),
		"max_queue_length":    intPtrOrDefault(cfg.MaxQueueLength, MAX_QUEUE_LENGTH),
		"retry_delay":         intPtrOrDefault(cfg.RetryDelay, RETRY_DELAY),
		"max_payload_size":    intPtrOrDefault(cfg.MaxPayloadSize, MAX_PAYLOAD_SIZE),
		"retry_enabled":       boolPtrOrDefault(cfg.RetryEnabled, RETRY_ENABLED),
	}
	m["message_ttl_days"] = float64(m["message_ttl_minutes"].(int)) / 1440.0
	return m
}

func getEnvWithOverride(key, envDefault, override string) string {
	if override != "" {
		return override
	}
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return envDefault
}

func intPtrOrDefault(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func boolPtrOrDefault(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

// GetEnvConfig returns the effective env config
func (cm *ConfigManager) GetEnvConfig() map[string]interface{} {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return BuildEnvConfig(cm.config.Env)
}

// UpdateEnvConfig saves overrides and applies them to globals
func (cm *ConfigManager) UpdateEnvConfig(env EnvParams) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.config.Env = env
	cm.save()

	// Apply to globals
	if env.QueuePrefix != "" { QUEUE_PREFIX = env.QueuePrefix }
	if env.RoutingPrefix != "" { ROUTING_PREFIX = env.RoutingPrefix }
	if env.ExchangeName != "" { EXCHANGE_NAME = env.ExchangeName }
	if env.MessageTTLMin != nil { MESSAGE_TTL_MINUTES = *env.MessageTTLMin }
	if env.MaxQueueLength != nil { MAX_QUEUE_LENGTH = *env.MaxQueueLength }
	if env.RetryEnabled != nil { RETRY_ENABLED = *env.RetryEnabled }
	if env.RetryDelay != nil { RETRY_DELAY = *env.RetryDelay }
	if env.DLXExchangeName != "" { DLX_EXCHANGE_NAME = env.DLXExchangeName }
	if env.BufferDir != "" { BUFFER_DIR = env.BufferDir }
	if env.MaxPayloadSize != nil { MAX_PAYLOAD_SIZE = *env.MaxPayloadSize }
	if env.NtfyURL != "" { NTFY_URL = env.NtfyURL }
	if env.RMQMgmtURL != "" { RMQ_MGMT_URL = env.RMQMgmtURL }
	if env.RMQMgmtUser != "" { RMQ_MGMT_USER = env.RMQMgmtUser }
	if env.RMQMgmtPassword != "" { RMQ_MGMT_PASSWORD = env.RMQMgmtPassword }

	log.Printf("Env config updated and applied")
	return nil
}

// RevertEnv clears all overrides and reloads from env
func (cm *ConfigManager) RevertEnv() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.config.Env = EnvParams{}
	if err := cm.save(); err != nil {
		return err
	}
	// Re-read env vars
	WEBHOOK_PORT = getEnv("WEBHOOK_PORT", "8001")
	QUEUE_PREFIX = getEnv("QUEUE_PREFIX", "wuzapi_")
	ROUTING_PREFIX = getEnv("ROUTING_PREFIX", "wa")
	EXCHANGE_NAME = getEnv("EXCHANGE_NAME", "wuzapi")
	MESSAGE_TTL_MINUTES = getEnvInt("MESSAGE_TTL_MINUTES", 4320)
	MAX_QUEUE_LENGTH = getEnvInt("MAX_QUEUE_LENGTH", 50000)
	RETRY_ENABLED = getEnvBool("RETRY_ENABLED", true)
	RETRY_DELAY = getEnvInt("RETRY_DELAY", 60)
	DLX_EXCHANGE_NAME = getEnv("DLX_EXCHANGE_NAME", "")
	BUFFER_DIR = getEnv("BUFFER_DIR", "./buffer")
	MAX_PAYLOAD_SIZE = 64 * 1024
	NTFY_URL = getEnv("NTFY_URL", "https://ntfy.sh/monitor-server-30AhxaPwq00MzspW")
	RMQ_MGMT_URL = getEnv("RMQ_MGMT_URL", "http://localhost:15672")
	RMQ_MGMT_USER = getEnv("RMQ_MGMT_USER", "guest")
	RMQ_MGMT_PASSWORD = getEnv("RMQ_MGMT_PASSWORD", "guest")
	SAVE_DIR = getEnv("SAVE_DIR", ".")

	log.Printf("Env config reverted to env defaults")
	return nil
}

func (cm *ConfigManager) UpsertRoute(sourceID, exchange, routingKey, queuePrefix, deviceFilter, filterField string, enabled bool, priority int) (Route, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	idx := -1
	for i := range cm.config.Sources {
		if cm.config.Sources[i].ID == sourceID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return Route{}, fmt.Errorf("source %s not found", sourceID)
	}

	r := Route{
		ID:           uuid.New().String()[:8],
		SourceID:     sourceID,
		Exchange:     exchange,
		RoutingKey:   routingKey,
		QueuePrefix:  queuePrefix,
		DeviceFilter: deviceFilter,
		FilterField:  filterField,
		Enabled:      enabled,
		Priority:     priority,
		CreatedAt:    time.Now().Unix(),
	}
	cm.config.Sources[idx].Routes = append(cm.config.Sources[idx].Routes, r)
	if err := cm.save(); err != nil {
		return Route{}, err
	}
	return r, nil
}

func (cm *ConfigManager) DeleteRoute(routeID string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for i := range cm.config.Sources {
		for j := range cm.config.Sources[i].Routes {
			if cm.config.Sources[i].Routes[j].ID == routeID {
				cm.config.Sources[i].Routes = append(
					cm.config.Sources[i].Routes[:j],
					cm.config.Sources[i].Routes[j+1:]...,
				)
				return cm.save()
			}
		}
	}
	return fmt.Errorf("route %s not found", routeID)
}
