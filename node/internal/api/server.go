package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cluster-os/node/internal/daemon"
	"github.com/sirupsen/logrus"
)

// Server provides HTTP API for cluster status and dashboard
type Server struct {
	daemon *daemon.Daemon
	logger *logrus.Logger
	port   int
	server *http.Server
}

// NewServer creates a new API server
func NewServer(d *daemon.Daemon, logger *logrus.Logger, port int) *Server {
	return &Server{
		daemon: d,
		logger: logger,
		port:   port,
	}
}

// Start starts the HTTP API server
func (s *Server) Start() error {
	mux := http.NewServeMux()
	
	// Dashboard/status endpoints
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	mux.HandleFunc("/api/v1/nodes", s.handleNodes)
	mux.HandleFunc("/api/v1/leaders", s.handleLeaders)
	mux.HandleFunc("/api/v1/services", s.handleServices)
	mux.HandleFunc("/api/v1/cluster", s.handleCluster)
	mux.HandleFunc("/api/v1/jobs", s.handleJobs)
	
	// Health check
	mux.HandleFunc("/health", s.handleHealth)
	
	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.port),
		Handler:      s.corsMiddleware(s.loggingMiddleware(mux)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	
	s.logger.Infof("Starting API server on port %d", s.port)
	
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Errorf("API server error: %v", err)
		}
	}()
	
	return nil
}

// Stop stops the HTTP API server
func (s *Server) Stop() error {
	if s.server != nil {
		s.logger.Info("Stopping API server")
		return s.server.Close()
	}
	return nil
}

// handleStatus returns comprehensive cluster status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	status := s.daemon.GetComprehensiveStatus()
	s.respondJSON(w, http.StatusOK, status)
}

// handleNodes returns information about all known nodes
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	nodes := s.daemon.GetAllNodes()
	s.respondJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(nodes),
		"nodes": nodes,
	})
}

// handleLeaders returns current leaders for each role
func (s *Server) handleLeaders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	leaders := s.daemon.GetAllLeaders()
	s.respondJSON(w, http.StatusOK, map[string]interface{}{
		"leaders": leaders,
	})
}

// handleServices returns status of all cluster services
func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	services := s.daemon.GetServiceStatus()
	s.respondJSON(w, http.StatusOK, map[string]interface{}{
		"services": services,
	})
}

// handleCluster returns cluster-wide information
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	cluster := s.daemon.GetClusterInfo()
	s.respondJSON(w, http.StatusOK, cluster)
}

// handleJobs returns information about running jobs
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	jobs := s.daemon.GetJobsInfo()
	s.respondJSON(w, http.StatusOK, map[string]interface{}{
		"jobs": jobs,
	})
}

// handleHealth returns simple health check
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"timestamp": time.Now().Unix(),
	})
}

// respondJSON sends a JSON response
func (s *Server) respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	
	if err := json.NewEncoder(w).Encode(data); err != nil {
		s.logger.Errorf("Failed to encode JSON response: %v", err)
	}
}

// loggingMiddleware logs HTTP requests
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Debugf("%s %s %s", r.Method, r.RequestURI, time.Since(start))
	})
}

// corsMiddleware adds CORS headers
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		next.ServeHTTP(w, r)
	})
}
