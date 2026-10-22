package metadata

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/ecs"
)

const statusRunning = "RUNNING"

type ECSLimits struct {
	CPU    *int64 `json:"CPU,omitempty"`
	Memory *int64 `json:"Memory,omitempty"`
}

type ECSContainerMetadata struct {
	DockerID      string                    `json:"DockerId"`
	Name          string                    `json:"Name"`
	DockerName    string                    `json:"DockerName"`
	Image         string                    `json:"Image"`
	ImageID       string                    `json:"ImageID"`
	Ports         []ecs.MetadataPortMapping `json:"Ports,omitempty"`
	Labels        map[string]string         `json:"Labels,omitempty"`
	DesiredStatus string                    `json:"DesiredStatus"`
	KnownStatus   string                    `json:"KnownStatus"`
	Limits        ECSLimits                 `json:"Limits"`
	CreatedAt     string                    `json:"CreatedAt,omitempty"`
	StartedAt     string                    `json:"StartedAt,omitempty"`
	Type          string                    `json:"Type"`
	Networks      []ecs.MetadataNetwork     `json:"Networks,omitempty"`
}

type ECSTaskMetadata struct {
	Limits           ECSLimits              `json:"Limits"`
	Cluster          string                 `json:"Cluster"`
	TaskARN          string                 `json:"TaskARN"`
	Family           string                 `json:"Family"`
	Revision         string                 `json:"Revision"`
	DesiredStatus    string                 `json:"DesiredStatus"`
	KnownStatus      string                 `json:"KnownStatus"`
	PullStartedAt    string                 `json:"PullStartedAt,omitempty"`
	PullStoppedAt    string                 `json:"PullStoppedAt,omitempty"`
	AvailabilityZone string                 `json:"AvailabilityZone,omitempty"`
	LaunchType       string                 `json:"LaunchType"`
	Containers       []ECSContainerMetadata `json:"Containers"`
}

type ECSContainerStats struct {
	Read        string         `json:"read"`
	PreRead     string         `json:"preread"`
	CPUStats    ECSCPUStats    `json:"cpu_stats"`
	PreCPUStats ECSCPUStats    `json:"precpu_stats"`
	MemoryStats ECSMemoryStats `json:"memory_stats"`
}

type ECSCPUStats struct {
	CPUUsage       ECSCPUUsage `json:"cpu_usage"`
	SystemCPUUsage int64       `json:"system_cpu_usage"`
	OnlineCPUs     int64       `json:"online_cpus"`
}

type ECSCPUUsage struct {
	TotalUsage        int64 `json:"total_usage"`
	UsageInKernelmode int64 `json:"usage_in_kernelmode"`
	UsageInUsermode   int64 `json:"usage_in_usermode"`
}

type ECSMemoryStats struct {
	Usage    int64 `json:"usage"`
	MaxUsage int64 `json:"max_usage"`
	Limit    int64 `json:"limit"`
}

type ecsErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ecsCredentialsResponse struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
	RoleArn         string `json:"RoleArn,omitempty"`
}

func (s *Service) serveECS(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	path = strings.TrimPrefix(path, "_tinystack/ecs/")
	path = strings.TrimPrefix(path, "_tinystack/ecs")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeECSError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed")
		return
	}

	switch {
	case path == "credentials" || strings.HasPrefix(path, "credentials/"):
		s.serveECSCredentials(w, r, strings.TrimPrefix(path, "credentials/"))

	case strings.HasPrefix(path, "v4/"):
		s.serveECSV4(w, r, strings.TrimPrefix(path, "v4/"))

	default:
		writeECSError(w, http.StatusNotFound, "ResourceNotFoundException", "not found")
	}
}

func (s *Service) serveECSCredentials(w http.ResponseWriter, r *http.Request, pathSuffix string) {
	token := r.Header.Get("Authorization")
	token = strings.TrimPrefix(token, "Bearer ")
	token = strings.TrimSpace(token)

	if token == "" && pathSuffix != "" && pathSuffix != "credentials" {
		token = pathSuffix
	}

	if token == "" {
		writeECSError(w, http.StatusUnauthorized, "AccessDenied", "missing authorization token")
		return
	}

	rec, ok, err := s.containers.ContainerMetadataByToken(r.Context(), token)
	if err != nil {
		writeECSError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	if !ok {
		writeECSError(w, http.StatusUnauthorized, "AccessDenied", "invalid authorization token")
		return
	}

	if rec.TaskRoleARN == "" {
		writeECSError(w, http.StatusNotFound, "ResourceNotFoundException", "task has no role")
		return
	}

	if s.assumeRole == nil {
		writeECSError(w, http.StatusInternalServerError, "InternalError", "STS not configured")
		return
	}

	sessionName := "ecs-task-" + rec.TaskID
	accessKey, secret, tok, expires, err := s.assumeRole(
		r.Context(),
		"ecs-tasks.amazonaws.com",
		rec.TaskRoleARN,
		sessionName,
		3600,
	)
	if err != nil {
		writeECSError(w, http.StatusBadRequest, "AccessDenied", err.Error())
		return
	}

	resp := ecsCredentialsResponse{
		AccessKeyID:     accessKey,
		SecretAccessKey: secret,
		Token:           tok,
		Expiration:      expires.UTC().Format(time.RFC3339),
		RoleArn:         rec.TaskRoleARN,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Service) serveECSV4(w http.ResponseWriter, r *http.Request, subpath string) {
	parts := strings.Split(strings.Trim(subpath, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeECSError(w, http.StatusNotFound, "ResourceNotFoundException", "missing token")
		return
	}

	token := parts[0]
	rec, ok, err := s.containers.ContainerMetadataByToken(r.Context(), token)
	if err != nil {
		writeECSError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	if !ok {
		writeECSError(w, http.StatusNotFound, "ResourceNotFoundException", "container not found")
		return
	}

	action := ""
	if len(parts) > 1 {
		action = strings.Join(parts[1:], "/")
	}

	switch action {
	case "":
		s.serveContainerMetadata(w, rec)

	case "task":
		s.serveTaskMetadata(w, rec)

	case "stats":
		s.serveContainerStats(w, rec)

	case "task/stats":
		s.serveTaskStats(w, rec)

	default:
		writeECSError(w, http.StatusNotFound, "ResourceNotFoundException", "unknown metadata path")
	}
}

func (s *Service) serveContainerMetadata(w http.ResponseWriter, rec *ecs.ContainerMetadata) {
	dockerID := rec.DockerID
	if dockerID == "" {
		dockerID = rec.ContainerName + "-" + rec.TaskID
	}

	dockerName := rec.DockerName
	if dockerName == "" {
		dockerName = "ecs-" + rec.Family + "-" + rec.Revision + "-" + rec.ContainerName
	}

	networks := rec.Networks
	if len(networks) == 0 {
		networks = []ecs.MetadataNetwork{
			{
				NetworkMode:   "default",
				IPv4Addresses: []string{"127.0.0.1"},
			},
		}
	}

	meta := ECSContainerMetadata{
		DockerID:      dockerID,
		Name:          rec.ContainerName,
		DockerName:    dockerName,
		Image:         rec.Image,
		ImageID:       rec.ImageID,
		Ports:         rec.Ports,
		Labels:        rec.Labels,
		DesiredStatus: statusRunning,
		KnownStatus:   statusRunning,
		Limits: ECSLimits{
			CPU:    rec.CPU,
			Memory: rec.Memory,
		},
		CreatedAt: rec.CreatedAt.UTC().Format(time.RFC3339),
		StartedAt: rec.StartedAt.UTC().Format(time.RFC3339),
		Type:      "NORMAL",
		Networks:  networks,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meta)
}

func (s *Service) serveTaskMetadata(w http.ResponseWriter, rec *ecs.ContainerMetadata) {
	dockerID := rec.DockerID
	if dockerID == "" {
		dockerID = rec.ContainerName + "-" + rec.TaskID
	}

	dockerName := rec.DockerName
	if dockerName == "" {
		dockerName = "ecs-" + rec.Family + "-" + rec.Revision + "-" + rec.ContainerName
	}

	networks := rec.Networks
	if len(networks) == 0 {
		networks = []ecs.MetadataNetwork{
			{
				NetworkMode:   "default",
				IPv4Addresses: []string{"127.0.0.1"},
			},
		}
	}

	container := ECSContainerMetadata{
		DockerID:      dockerID,
		Name:          rec.ContainerName,
		DockerName:    dockerName,
		Image:         rec.Image,
		ImageID:       rec.ImageID,
		Ports:         rec.Ports,
		Labels:        rec.Labels,
		DesiredStatus: statusRunning,
		KnownStatus:   statusRunning,
		Limits: ECSLimits{
			CPU:    rec.CPU,
			Memory: rec.Memory,
		},
		CreatedAt: rec.CreatedAt.UTC().Format(time.RFC3339),
		StartedAt: rec.StartedAt.UTC().Format(time.RFC3339),
		Type:      "NORMAL",
		Networks:  networks,
	}

	task := ECSTaskMetadata{
		Cluster:       rec.Cluster,
		TaskARN:       rec.TaskARN,
		Family:        rec.Family,
		Revision:      rec.Revision,
		DesiredStatus: statusRunning,
		KnownStatus:   statusRunning,
		Containers:    []ECSContainerMetadata{container},
		Limits: ECSLimits{
			CPU:    rec.CPU,
			Memory: rec.Memory,
		},
		PullStartedAt:    rec.CreatedAt.UTC().Format(time.RFC3339),
		PullStoppedAt:    rec.StartedAt.UTC().Format(time.RFC3339),
		AvailabilityZone: "us-east-1a",
		LaunchType:       "EC2",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(task)
}

func (s *Service) serveContainerStats(w http.ResponseWriter, rec *ecs.ContainerMetadata) {
	nowStr := s.now().UTC().Format(time.RFC3339Nano)
	stats := ECSContainerStats{
		Read:    nowStr,
		PreRead: nowStr,
		CPUStats: ECSCPUStats{
			CPUUsage: ECSCPUUsage{
				TotalUsage:        0,
				UsageInKernelmode: 0,
				UsageInUsermode:   0,
			},
			SystemCPUUsage: 0,
			OnlineCPUs:     1,
		},
		PreCPUStats: ECSCPUStats{
			CPUUsage: ECSCPUUsage{
				TotalUsage:        0,
				UsageInKernelmode: 0,
				UsageInUsermode:   0,
			},
			SystemCPUUsage: 0,
			OnlineCPUs:     1,
		},
		MemoryStats: ECSMemoryStats{
			Usage:    0,
			MaxUsage: 0,
			Limit:    536870912,
		},
	}

	if rec.Memory != nil && *rec.Memory > 0 {
		stats.MemoryStats.Limit = *rec.Memory * 1024 * 1024
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

func (s *Service) serveTaskStats(w http.ResponseWriter, rec *ecs.ContainerMetadata) {
	dockerID := rec.DockerID
	if dockerID == "" {
		dockerID = rec.ContainerName + "-" + rec.TaskID
	}

	nowStr := s.now().UTC().Format(time.RFC3339Nano)
	stats := ECSContainerStats{
		Read:    nowStr,
		PreRead: nowStr,
		CPUStats: ECSCPUStats{
			CPUUsage: ECSCPUUsage{
				TotalUsage:        0,
				UsageInKernelmode: 0,
				UsageInUsermode:   0,
			},
			SystemCPUUsage: 0,
			OnlineCPUs:     1,
		},
		PreCPUStats: ECSCPUStats{
			CPUUsage: ECSCPUUsage{
				TotalUsage:        0,
				UsageInKernelmode: 0,
				UsageInUsermode:   0,
			},
			SystemCPUUsage: 0,
			OnlineCPUs:     1,
		},
		MemoryStats: ECSMemoryStats{
			Usage:    0,
			MaxUsage: 0,
			Limit:    536870912,
		},
	}

	if rec.Memory != nil && *rec.Memory > 0 {
		stats.MemoryStats.Limit = *rec.Memory * 1024 * 1024
	}

	taskStats := map[string]ECSContainerStats{
		dockerID: stats,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(taskStats)
}

func writeECSError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ecsErrorResponse{
		Code:    code,
		Message: message,
	})
}
