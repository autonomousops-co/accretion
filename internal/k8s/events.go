package k8s

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EventEntry represents a Kubernetes event relevant to deployment and config changes.
type EventEntry struct {
	Timestamp      time.Time
	Namespace      string
	Name           string
	Reason         string
	Message        string
	InvolvedObject string // e.g. "Deployment/my-app"
	Kind           string // kind of the involved object
	Source         string // component that reported the event
	Type           string // Normal or Warning
	Count          int32
}

// ListEvents retrieves events from a namespace that occurred after the given time.
func (c *Client) ListEvents(ctx context.Context, namespace string, since time.Time) ([]EventEntry, error) {
	list, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list events in namespace %s: %w", namespace, err)
	}

	entries := make([]EventEntry, 0, len(list.Items))
	for i := range list.Items {
		ev := &list.Items[i]
		// Use LastTimestamp if available, otherwise EventTime, otherwise CreationTimestamp
		ts := ev.LastTimestamp.Time
		if ts.IsZero() {
			ts = ev.EventTime.Time
		}
		if ts.IsZero() {
			ts = ev.CreationTimestamp.Time
		}

		if !since.IsZero() && ts.Before(since) {
			continue
		}

		involvedObj := fmt.Sprintf("%s/%s", ev.InvolvedObject.Kind, ev.InvolvedObject.Name)

		entries = append(entries, EventEntry{
			Timestamp:      ts,
			Namespace:      ev.Namespace,
			Name:           ev.Name,
			Reason:         ev.Reason,
			Message:        ev.Message,
			InvolvedObject: involvedObj,
			Kind:           ev.InvolvedObject.Kind,
			Source:         ev.Source.Component,
			Type:           ev.Type,
			Count:          ev.Count,
		})
	}

	return entries, nil
}
