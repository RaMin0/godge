package godge

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"

	docker "github.com/fsouza/go-dockerclient"
)

type users map[string]string

type Server struct {
	address            string
	tasks              map[string]Task
	pendingSubmissions chan submissionRequest
	requestErrorChan   chan error
	dockerClient       *docker.Client
	users              users
	scoreboard         scoreboard
}

func NewServer(address string, dockerAddress string) (*Server, error) {
	dc, err := docker.NewClient(dockerAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to docker daemon: %v", err)
	}
	if err := dc.Ping(); err != nil {
		return nil, fmt.Errorf("failed to connect to docker daemon: %v", err)
	}
	return &Server{
		address:            address,
		tasks:              make(map[string]Task),
		pendingSubmissions: make(chan submissionRequest),
		dockerClient:       dc,
		users:              make(users),
		scoreboard:         make(scoreboard),
	}, nil
}

func (s *Server) RegisterTask(t Task) {
	s.tasks[t.Name] = t
}

func (s *Server) handleSubmission(sub *Submission) error {
	t, ok := s.tasks[sub.TaskName]
	if !ok {
		return fmt.Errorf("task %v not found", sub.TaskName)
	}
	if err := t.Execute(sub); err != nil {
		return fmt.Errorf("task %v failed: %v", sub.TaskName, err)
	}
	return nil
}

type submissionRequest struct {
	result     chan error
	submission *Submission
}

func (s *Server) reportResult(sub *Submission, err error) {
	log.Printf("%v submission for %v: %v", sub.Language, sub.TaskName, err)
	if err != nil {
		s.scoreboard.set(sub.Username, sub.TaskName, failedVerdict)
		return
	}
	s.scoreboard.set(sub.Username, sub.TaskName, passedVerdict)
}

func (s *Server) processSubmissions() {
	for sreq := range s.pendingSubmissions {
		err := s.handleSubmission(sreq.submission)
		sreq.result <- err
		s.reportResult(sreq.submission, err)
	}
}

type SubmissionResponse struct {
	Passed bool   `json:"passed"`
	Error  string `json:"error"`
}

func (s *Server) submitHTTPHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		httpJSONError(w, "Only POST requests are allowed", http.StatusMethodNotAllowed)
		return
	}
	if username, password, ok := req.BasicAuth(); !ok || s.users[username] != password {
		httpJSONError(w, "Wrong username or password", http.StatusUnauthorized)
		return
	}
	var sub Submission
	err := json.NewDecoder(req.Body).Decode(&sub)
	if err != nil {
		httpJSONError(w, fmt.Sprintf("Failed to decode request body: %v", err), http.StatusBadRequest)
		return
	}
	sub.Executor.setDockerClient(s.dockerClient)

	res := make(chan error)
	s.pendingSubmissions <- submissionRequest{
		result:     res,
		submission: &sub,
	}

	w.WriteHeader(http.StatusOK)
	result := <-res

	resp := SubmissionResponse{
		Passed: true,
		Error:  "",
	}

	if result != nil {
		resp = SubmissionResponse{
			Passed: false,
			Error:  result.Error(),
		}
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		httpJSONError(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) registerHTTPHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		httpJSONError(w, "Only POST requests are allowed", http.StatusMethodNotAllowed)
		return
	}

	var rreq RegisterRequest
	err := json.NewDecoder(req.Body).Decode(&rreq)
	if err != nil {
		httpJSONError(w, fmt.Sprintf("Failed to decode request body: %v", err), http.StatusBadRequest)
		return
	}

	if len(rreq.Username) == 0 {
		httpJSONError(w, fmt.Sprintf("Username cannot be empty"), http.StatusBadRequest)
		return
	}

	if _, ok := s.users[rreq.Username]; ok {
		httpJSONError(w, fmt.Sprintf("Username %v is already registered", rreq.Username), http.StatusBadRequest)
		return
	}

	s.users[rreq.Username] = rreq.Password
	log.Printf("User %v registered", rreq.Username)

	w.WriteHeader(http.StatusCreated)
}

func (s *Server) tasksHTTPHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		httpJSONError(w, "Only GET requests are allowed", http.StatusMethodNotAllowed)
		return
	}

	var ts []Task

	for _, t := range s.tasks {
		ts = append(ts, t)
	}

	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(ts); err != nil {
		httpJSONError(w, "Failed to encode tasks", http.StatusInternalServerError)
		return
	}
}

func (s *Server) scoreboardHTTPHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		httpJSONError(w, "Only GET requests are allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Add("Content-Type", "text/html")

	var ts []string
	for k := range s.tasks {
		ts = append(ts, k)
	}
	sort.Strings(ts)

	var us []string
	for k := range s.users {
		us = append(us, k)
	}
	sort.Strings(us)

	scoreboard := s.scoreboard.toScoreboard(us, ts)

	scoreboardTmpl.Execute(w, map[string]interface{}{
		"Scoreboard": scoreboard,
	})
}

func (s *Server) Start() error {
	go s.processSubmissions()
	mux := http.NewServeMux()
	mux.HandleFunc("/submit", s.submitHTTPHandler)
	mux.HandleFunc("/register", s.registerHTTPHandler)
	mux.HandleFunc("/tasks", s.tasksHTTPHandler)
	mux.HandleFunc("/scoreboard", s.scoreboardHTTPHandler)
	return http.ListenAndServe(s.address, mux)
}
