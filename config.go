package main

import (
	"encoding/json"
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

type Config struct {
	Version int       `json:"version"`
	RMQ     RMQConfig `json:"rabbitmq"`
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

func (cm *ConfigManager) save() {
	data, err := json.MarshalIndent(cm.config, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal config: %v", err)
		return
	}
	// Atomic write: write to .tmp then rename
	tmp := cm.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("Failed to write config: %v", err)
		return
	}
	if err := os.Rename(tmp, cm.path); err != nil {
		log.Printf("Failed to atomically save config: %v", err)
		return
	}
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
			// Update existing
			cm.config.Sources[i].Path = path
			cm.config.Sources[i].AuthToken = authToken
			cm.save()
			return cm.config.Sources[i], nil
		}
	}

	// Create new
	s := Source{
		ID:        uuid.New().String()[:8],
		Name:      name,
		Path:      path,
		AuthToken: authToken,
		Routes:    make([]Route, 0),
		CreatedAt: time.Now().Unix(),
	}
	cm.config.Sources = append(cm.config.Sources, s)
	cm.save()
	return s, nil
}

func (cm *ConfigManager) DeleteSource(id string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for i := range cm.config.Sources {
		if cm.config.Sources[i].ID == id {
			cm.config.Sources = append(cm.config.Sources[:i], cm.config.Sources[i+1:]...)
			cm.save()
			return true
		}
	}
	return false
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

func (cm *ConfigManager) UpdateRMQ(rmq RMQConfig) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.config.RMQ = rmq
	applyRMQToGlobals(rmq)
	cm.save()
	log.Printf("RMQ config updated: %s:%s exchange=%s", rmq.Host, rmq.Port, rmq.Exchange)
}

func (cm *ConfigManager) UpsertRoute(sourceID, exchange, routingKey, queuePrefix, deviceFilter, filterField string, enabled bool, priority int) (Route, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Find source
	idx := -1
	for i := range cm.config.Sources {
		if cm.config.Sources[i].ID == sourceID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return Route{}, nil
	}

	// Create new route
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
	cm.save()
	return r, nil
}

func (cm *ConfigManager) DeleteRoute(routeID string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for i := range cm.config.Sources {
		for j := range cm.config.Sources[i].Routes {
			if cm.config.Sources[i].Routes[j].ID == routeID {
				cm.config.Sources[i].Routes = append(
					cm.config.Sources[i].Routes[:j],
					cm.config.Sources[i].Routes[j+1:]...,
				)
				cm.save()
				return true
			}
		}
	}
	return false
}
