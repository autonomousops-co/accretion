package k8s

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodInfo represents pod information needed by the controller
type PodInfo struct {
	UID        string
	Name       string
	Namespace  string
	NodeName   string
	Phase      string
	Containers []ContainerInfo
}

// ContainerInfo represents container information
type ContainerInfo struct {
	Name string
}

// LogEntry represents a log entry from a pod
type LogEntry struct {
	Timestamp time.Time
	Namespace string
	PodName   string
	Container string
	Message   string
}

// GetPods retrieves pods from a namespace
func (c *Client) GetPods(ctx context.Context, namespace string) (*corev1.PodList, error) {
	return c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
}

// GetPod retrieves a specific pod by name
func (c *Client) GetPod(ctx context.Context, namespace, podName string) (*corev1.Pod, error) {
	return c.clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
}

// IsPodRunningByName checks if a pod is running by namespace and name
func (c *Client) IsPodRunningByName(ctx context.Context, namespace, podName string) bool {
	pod, err := c.GetPod(ctx, namespace, podName)
	if err != nil {
		return false
	}

	podInfo, err := c.ExtractPodInfo(pod)
	if err != nil {
		return false
	}

	return c.IsPodRunning(podInfo)
}

// GetPodLogs retrieves logs from a pod container
func (c *Client) GetPodLogs(
	ctx context.Context,
	namespace, podName, containerName string,
	tailLines int64,
) ([]LogEntry, error) {
	options := &corev1.PodLogOptions{
		Container:  containerName,
		TailLines:  &tailLines,
		Timestamps: true,
	}

	req := c.clientset.CoreV1().Pods(namespace).GetLogs(podName, options)
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod logs: %w", err)
	}
	defer podLogs.Close()

	buf, err := io.ReadAll(podLogs)
	if err != nil {
		return nil, fmt.Errorf("failed to read pod logs: %w", err)
	}

	return parseLogEntries(string(buf), namespace, podName, containerName), nil
}

// StreamPodLogs streams logs from a pod container
func (c *Client) StreamPodLogs(
	ctx context.Context,
	namespace, podName, containerName string,
	logChan chan<- LogEntry,
) error {
	options := &corev1.PodLogOptions{
		Container:  containerName,
		Follow:     true,
		Timestamps: true,
	}

	req := c.clientset.CoreV1().Pods(namespace).GetLogs(podName, options)
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("failed to stream pod logs: %w", err)
	}
	defer podLogs.Close()

	return streamLogEntries(ctx, podLogs, namespace, podName, containerName, logChan)
}

// streamLogEntries reads r one complete line at a time and emits a LogEntry per
// line.
//
// Reading whole lines rather than fixed-size byte chunks is load-bearing: a line
// split across two reads would leave the tail fragment without its kubelet
// timestamp prefix, which both mangles the message and forces parseLogLine onto
// its non-deterministic time.Now() fallback — breaking (content_hash, timestamp)
// dedup for that fragment.
func streamLogEntries(
	ctx context.Context,
	r io.Reader,
	namespace, podName, containerName string,
	logChan chan<- LogEntry,
) error {
	reader := bufio.NewReader(r)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// ReadString returns any data read before the error, so a trailing line
		// with no newline is still emitted before we stop on EOF.
		line, err := reader.ReadString('\n')
		if line != "" {
			for _, entry := range parseLogEntries(
				strings.TrimRight(line, "\r\n"), namespace, podName, containerName,
			) {
				select {
				case logChan <- entry:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("failed to read pod logs: %w", err)
		}
	}
}

// ExtractPodInfo extracts PodInfo from a Kubernetes object
func (c *Client) ExtractPodInfo(obj interface{}) (*PodInfo, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("failed to cast object to Pod, got type: %T", obj)
	}

	containers := make([]ContainerInfo, len(pod.Spec.Containers))
	for i := range pod.Spec.Containers {
		containers[i] = ContainerInfo{Name: pod.Spec.Containers[i].Name}
	}

	return &PodInfo{
		UID:        string(pod.UID),
		Name:       pod.Name,
		Namespace:  pod.Namespace,
		NodeName:   pod.Spec.NodeName,
		Phase:      string(pod.Status.Phase),
		Containers: containers,
	}, nil
}

// IsPodRunning checks if a pod is in running state
func (c *Client) IsPodRunning(podInfo *PodInfo) bool {
	return podInfo.Phase == string(corev1.PodRunning)
}

// GeneratePodKey generates a pod key from PodInfo
func (c *Client) GeneratePodKey(podInfo *PodInfo) (string, error) {
	if podInfo.Namespace == "" || podInfo.Name == "" {
		return "", fmt.Errorf("pod namespace and name cannot be empty")
	}
	return fmt.Sprintf("%s/%s", podInfo.Namespace, podInfo.Name), nil
}

// parseLogEntries parses log data into LogEntry structs
func parseLogEntries(logData, namespace, podName, containerName string) []LogEntry {
	// This is a simplified parser - in production you'd want more robust parsing
	lines := splitLines(logData)
	entries := make([]LogEntry, 0, len(lines))

	for _, line := range lines {
		if line == "" {
			continue
		}

		ts, msg := parseLogLine(line)
		entry := LogEntry{
			Namespace: namespace,
			PodName:   podName,
			Container: containerName,
			Message:   msg,
			Timestamp: ts,
		}

		entries = append(entries, entry)
	}

	return entries
}

// parseLogLine splits a kubelet log line ("<RFC3339Nano> <message>", produced
// because PodLogOptions.Timestamps is true) into its event timestamp and the
// message body. Using the real per-line time as the entry timestamp — instead of
// time.Now() — keeps (content_hash, timestamp) stable across re-fetches of the
// same line (overlapping windows / restarts) so it dedupes rather than inserting
// a duplicate. Lines without a parseable prefix fall back to ingest time.
func parseLogLine(line string) (time.Time, string) {
	if idx := strings.IndexByte(line, ' '); idx > 0 {
		if ts, err := time.Parse(time.RFC3339Nano, line[:idx]); err == nil {
			return ts, line[idx+1:]
		}
	}
	return time.Now(), line
}

// splitLines splits a string into lines
func splitLines(s string) []string {
	if s == "" {
		return nil
	}

	lines := make([]string, 0)
	start := 0

	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}

	if start < len(s) {
		lines = append(lines, s[start:])
	}

	return lines
}
