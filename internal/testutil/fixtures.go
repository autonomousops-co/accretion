package testutil

import (
	"fmt"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/llm"
)

// TestConfig creates a test configuration with sensible defaults.
func TestConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Host: "localhost",
		},
		API: config.APIConfig{Enabled: true, Port: 41337},
		UI:  config.UIConfig{Enabled: false, Port: 8080},
		Database: config.DatabaseConfig{
			Host:     "localhost",
			Port:     5432,
			User:     "test_user",
			Password: "test_password",
			Database: "test_db",
			SSLMode:  "disable",
		},
		LLM: config.LLMConfig{
			Provider: "openai",
			OpenAI: config.OpenAIConfig{
				APIKey:  "test-api-key",
				BaseURL: "https://api.openai.com/v1",
			},
			Ollama: config.OllamaConfig{
				BaseURL: "http://localhost:11434",
			},
			EmbeddingModel: "nomic-embed-text:latest",
			ChatModel:      "llama3:8b",
		},
		K8s: config.K8sConfig{
			ConfigPath:   "",
			Namespaces:   []string{"default", "test-namespace"},
			PollInterval: "10s",
		},
		Logging: config.LoggingConfig{
			Level:  "debug",
			Format: "console",
		},
	}
}

// TestCollectorConfig creates a test collector configuration with sensible defaults.
func TestCollectorConfig() *config.CollectorConfig {
	return &config.CollectorConfig{
		Database: config.DatabaseConfig{
			Host:     "localhost",
			Port:     5432,
			User:     "test_user",
			Password: "test_password",
			Database: "test_db",
			SSLMode:  "disable",
		},
		LLM: config.LLMConfig{
			Provider: "openai",
			OpenAI: config.OpenAIConfig{
				APIKey:  "test-api-key",
				BaseURL: "https://api.openai.com/v1",
			},
			Ollama: config.OllamaConfig{
				BaseURL: "http://localhost:11434",
			},
			EmbeddingModel: "nomic-embed-text:latest",
		},
		K8s: config.K8sConfig{
			ConfigPath:   "",
			Namespaces:   []string{"default", "test-namespace"},
			PollInterval: "10s",
		},
		Logging: config.LoggingConfig{
			Level:  "debug",
			Format: "console",
		},
	}
}

// TestLogEntry creates a test log entry with default values.
func TestLogEntry(namespace, podName, container, message string) database.LogEntry {
	return database.LogEntry{
		ID:          1,
		Namespace:   namespace,
		PodName:     podName,
		Container:   container,
		Message:     message,
		Timestamp:   time.Now(),
		Embedding:   TestEmbedding(),
		CreatedAt:   time.Now(),
		ContentHash: TestContentHash(namespace, podName, container, message),
	}
}

// TestMetricsEntry creates a test metrics entry with default values.
func TestMetricsEntry(namespace, podName, container string) database.MetricsEntry {
	return database.MetricsEntry{
		ID:        1,
		Namespace: namespace,
		PodName:   podName,
		Container: container,
		CPU:       "100m",
		Memory:    "256Mi",
		Timestamp: time.Now(),
		Embedding: TestEmbedding(),
		CreatedAt: time.Now(),
	}
}

// TestK8sLogEntry creates a test Kubernetes log entry.
func TestK8sLogEntry(namespace, podName, container, message string) k8s.LogEntry {
	return k8s.LogEntry{
		Timestamp: time.Now(),
		Namespace: namespace,
		PodName:   podName,
		Container: container,
		Message:   message,
	}
}

// TestK8sPodMetrics creates test Kubernetes pod metrics.
func TestK8sPodMetrics(namespace, podName string, containers map[string]k8s.ContainerMetrics) k8s.PodMetrics {
	containerMetrics := make([]k8s.ContainerMetrics, 0, len(containers))
	for name, metrics := range containers {
		metrics.Name = name
		containerMetrics = append(containerMetrics, metrics)
	}

	return k8s.PodMetrics{
		Namespace:  namespace,
		PodName:    podName,
		Containers: containerMetrics,
		Timestamp:  time.Now(),
	}
}

// TestK8sContainerMetrics creates test container metrics.
func TestK8sContainerMetrics(name, cpu, memory string) k8s.ContainerMetrics {
	return k8s.ContainerMetrics{
		Name:   name,
		CPU:    cpu,
		Memory: memory,
	}
}

// TestPod creates a test Kubernetes pod.
func TestPod(namespace, name string, containers []string) corev1.Pod {
	containerSpecs := make([]corev1.Container, 0, len(containers))
	for _, containerName := range containers {
		containerSpecs = append(containerSpecs, corev1.Container{
			Name:  containerName,
			Image: "test-image:latest",
		})
	}

	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app": "test-app",
			},
		},
		Spec: corev1.PodSpec{
			Containers: containerSpecs,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// TestLLMMessage creates a test LLM message.
func TestLLMMessage(role, content string) llm.Message {
	return llm.Message{
		Role:    role,
		Content: content,
	}
}

// TestLLMResponse creates a test LLM response.
func TestLLMResponse(content string) llm.Response {
	return llm.Response{
		Content: content,
		Usage: llm.Usage{
			PromptTokens:     20,
			CompletionTokens: 15,
			TotalTokens:      35,
		},
	}
}

// TestSearchResult creates a test database search result.
func TestSearchResult(
	logEntry *database.LogEntry,
	metricsEntry *database.MetricsEntry,
	similarity float64,
) database.SearchResult {
	return database.SearchResult{
		LogEntry:     logEntry,
		MetricsEntry: metricsEntry,
		Similarity:   similarity,
	}
}

// TestEmbedding creates a test embedding vector.
func TestEmbedding() pgvector.Vector {
	// Create a simple test embedding
	embedding := make([]float32, 768)
	for i := 0; i < 768; i++ {
		embedding[i] = float32(i) / 768.0
	}
	return pgvector.NewVector(embedding)
}

// TestEmbeddingFromSeed creates a deterministic test embedding based on a seed string.
func TestEmbeddingFromSeed(seed string) pgvector.Vector {
	embedding := make([]float32, 768)
	seedBytes := []byte(seed)
	seedLen := len(seedBytes)

	for i := 0; i < 768; i++ {
		if seedLen > 0 {
			byteIndex := i % seedLen
			embedding[i] = float32(seedBytes[byteIndex]) / 255.0
		} else {
			embedding[i] = float32(i) / 768.0
		}
	}

	return pgvector.NewVector(embedding)
}

// TestContentHash creates a test content hash for log deduplication.
func TestContentHash(namespace, podName, container, message string) string {
	return fmt.Sprintf("test-hash-%s-%s-%s-%d", namespace, podName, container, len(message))
}

// Common test data sets

// TestLogEntries returns a set of test log entries for various testing scenarios.
func TestLogEntries() []database.LogEntry {
	return []database.LogEntry{
		TestLogEntry("default", "web-server-1", "nginx", "HTTP request: GET /api/health"),
		TestLogEntry("default", "web-server-1", "nginx", "HTTP request: POST /api/users"),
		TestLogEntry("default", "web-server-2", "nginx", "Connection timeout to upstream"),
		TestLogEntry("default", "api-server-1", "app", "Database connection established"),
		TestLogEntry("default", "api-server-1", "app", "Processing user request"),
		TestLogEntry("kube-system", "coredns-1", "coredns", "DNS query for example.com"),
		TestLogEntry("monitoring", "prometheus-1", "prometheus", "Scraping metrics from targets"),
	}
}

// TestMetricsEntries returns a set of test metrics entries.
func TestMetricsEntries() []database.MetricsEntry {
	return []database.MetricsEntry{
		TestMetricsEntry("default", "web-server-1", "nginx"),
		TestMetricsEntry("default", "api-server-1", "app"),
		TestMetricsEntry("kube-system", "coredns-1", "coredns"),
	}
}

// TestPods returns a set of test Kubernetes pods.
func TestPods() []corev1.Pod {
	return []corev1.Pod{
		TestPod("default", "web-server-1", []string{"nginx", "sidecar"}),
		TestPod("default", "web-server-2", []string{"nginx"}),
		TestPod("default", "api-server-1", []string{"app", "redis"}),
		TestPod("kube-system", "coredns-1", []string{"coredns"}),
	}
}

// TestLLMMessages returns a set of test conversation messages.
func TestLLMMessages() []llm.Message {
	return []llm.Message{
		TestLLMMessage("system", "You are a helpful Kubernetes troubleshooting assistant."),
		TestLLMMessage("user", "What errors are happening in my cluster?"),
		TestLLMMessage("assistant", "Based on the logs, I can see some connection timeouts in your web servers."),
		TestLLMMessage("user", "How can I fix these connection issues?"),
	}
}

// Test scenarios for different error conditions

// ErrorScenarios contains different error scenarios for testing.
type ErrorScenarios struct {
	DatabaseUnavailable bool
	LLMServiceDown      bool
	K8sAPIUnreachable   bool
	InvalidConfig       bool
}

// TestErrorConfig creates a configuration that will cause various errors.
func TestErrorConfig(scenario ErrorScenarios) *config.Config {
	cfg := TestConfig()

	if scenario.DatabaseUnavailable {
		cfg.Database.Host = "unreachable-host"
		cfg.Database.Port = 9999
	}

	if scenario.LLMServiceDown {
		cfg.LLM.OpenAI.BaseURL = "http://unreachable-llm-service"
		cfg.LLM.Ollama.BaseURL = "http://unreachable-ollama-service"
	}

	if scenario.InvalidConfig {
		cfg.K8s.PollInterval = "invalid-duration"
		cfg.Database.Port = 0
	}

	return cfg
}

// TestCollectorErrorConfig creates a collector configuration that will cause various errors.
func TestCollectorErrorConfig(scenario ErrorScenarios) *config.CollectorConfig {
	cfg := TestCollectorConfig()

	if scenario.DatabaseUnavailable {
		cfg.Database.Host = "unreachable-host"
		cfg.Database.Port = 9999
	}

	if scenario.LLMServiceDown {
		cfg.LLM.OpenAI.BaseURL = "http://unreachable-llm-service"
		cfg.LLM.Ollama.BaseURL = "http://unreachable-ollama-service"
	}

	if scenario.InvalidConfig {
		cfg.K8s.PollInterval = "invalid-duration"
		cfg.Database.Port = 0
	}

	return cfg
}

// Performance testing utilities

// BenchmarkData contains data sets for performance testing.
type BenchmarkData struct {
	LogEntries     []database.LogEntry
	MetricsEntries []database.MetricsEntry
	Embeddings     []pgvector.Vector
}

// GenerateBenchmarkData creates large data sets for performance testing.
func GenerateBenchmarkData(logCount, metricsCount int) BenchmarkData {
	data := BenchmarkData{
		LogEntries:     make([]database.LogEntry, logCount),
		MetricsEntries: make([]database.MetricsEntry, metricsCount),
		Embeddings:     make([]pgvector.Vector, logCount+metricsCount),
	}

	// Generate log entries
	for i := 0; i < logCount; i++ {
		data.LogEntries[i] = TestLogEntry(
			fmt.Sprintf("namespace-%d", i%10),
			fmt.Sprintf("pod-%d", i%100),
			fmt.Sprintf("container-%d", i%5),
			fmt.Sprintf("Log message %d with some content", i),
		)
		data.Embeddings[i] = TestEmbeddingFromSeed(fmt.Sprintf("log-%d", i))
	}

	// Generate metrics entries
	for i := 0; i < metricsCount; i++ {
		data.MetricsEntries[i] = TestMetricsEntry(
			fmt.Sprintf("namespace-%d", i%10),
			fmt.Sprintf("pod-%d", i%100),
			fmt.Sprintf("container-%d", i%5),
		)
		data.Embeddings[logCount+i] = TestEmbeddingFromSeed(fmt.Sprintf("metrics-%d", i))
	}

	return data
}

// Assertion helpers

// AssertLogEntry compares two log entries for equality in tests.
func AssertLogEntry(t *testing.T, expected, actual *database.LogEntry) {
	if expected.Namespace != actual.Namespace {
		t.Errorf("Expected namespace %s, got %s", expected.Namespace, actual.Namespace)
	}
	if expected.PodName != actual.PodName {
		t.Errorf("Expected pod name %s, got %s", expected.PodName, actual.PodName)
	}
	if expected.Container != actual.Container {
		t.Errorf("Expected container %s, got %s", expected.Container, actual.Container)
	}
	if expected.Message != actual.Message {
		t.Errorf("Expected message %s, got %s", expected.Message, actual.Message)
	}
}

// AssertMetricsEntry compares two metrics entries for equality in tests.
func AssertMetricsEntry(t *testing.T, expected, actual *database.MetricsEntry) {
	if expected.Namespace != actual.Namespace {
		t.Errorf("Expected namespace %s, got %s", expected.Namespace, actual.Namespace)
	}
	if expected.PodName != actual.PodName {
		t.Errorf("Expected pod name %s, got %s", expected.PodName, actual.PodName)
	}
	if expected.Container != actual.Container {
		t.Errorf("Expected container %s, got %s", expected.Container, actual.Container)
	}
	if expected.CPU != actual.CPU {
		t.Errorf("Expected CPU %s, got %s", expected.CPU, actual.CPU)
	}
	if expected.Memory != actual.Memory {
		t.Errorf("Expected memory %s, got %s", expected.Memory, actual.Memory)
	}
}
