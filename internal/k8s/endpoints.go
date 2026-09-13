package k8s

import (
	"fmt"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
)

// EndpointConditionInfo captures the condition of a single endpoint.
type EndpointConditionInfo struct {
	// Ready indicates whether this endpoint is ready to receive traffic.
	Ready bool
	// Serving indicates whether this endpoint is able to serve traffic (may be true even during termination).
	Serving bool
	// Terminating indicates whether this endpoint is terminating.
	Terminating bool
}

// EndpointInfo captures a single endpoint address and its state.
type EndpointInfo struct {
	TargetRef  string
	NodeName   string
	Addresses  []string
	Conditions EndpointConditionInfo
}

// EndpointHealthEntry represents the health snapshot of endpoints for a service.
type EndpointHealthEntry struct {
	// Timestamp is when the status was captured.
	Timestamp time.Time
	// Namespace is the Kubernetes namespace.
	Namespace string
	// ServiceName is the name of the service these endpoints belong to.
	ServiceName string
	// Endpoints are the individual endpoint states.
	Endpoints []EndpointInfo
	// ReadyCount is the number of ready endpoints.
	ReadyCount int
	// NotReadyCount is the number of not-ready endpoints.
	NotReadyCount int
	// TotalCount is the total number of endpoints.
	TotalCount int
}

// buildEndpointHealthEntry builds an EndpointHealthEntry from a set of EndpointSlices for one service.
func buildEndpointHealthEntry(
	namespace, serviceName string, slices []discoveryv1.EndpointSlice, now time.Time,
) EndpointHealthEntry {
	entry := EndpointHealthEntry{
		Timestamp:   now,
		Namespace:   namespace,
		ServiceName: serviceName,
	}

	// No single owning object; use the slices' creation times as the deterministic
	// timestamp so an unchanged endpoint set dedupes across polls/restarts. When
	// endpoints change, the content hash moves and a new row is written regardless.
	sliceTimes := make([]time.Time, 0, len(slices))
	for si := range slices {
		sliceTimes = append(sliceTimes, slices[si].CreationTimestamp.Time)
	}
	entry.Timestamp = deterministicStatusTime(nil, now, sliceTimes...)

	for si := range slices {
		for i := range slices[si].Endpoints {
			ep := &slices[si].Endpoints[i]
			info := EndpointInfo{
				Addresses: ep.Addresses,
			}

			if ep.Conditions.Ready != nil {
				info.Conditions.Ready = *ep.Conditions.Ready
			}
			if ep.Conditions.Serving != nil {
				info.Conditions.Serving = *ep.Conditions.Serving
			}
			if ep.Conditions.Terminating != nil {
				info.Conditions.Terminating = *ep.Conditions.Terminating
			}

			if ep.TargetRef != nil {
				info.TargetRef = fmt.Sprintf("%s/%s", ep.TargetRef.Kind, ep.TargetRef.Name)
			}
			if ep.NodeName != nil {
				info.NodeName = *ep.NodeName
			}

			entry.Endpoints = append(entry.Endpoints, info)
			entry.TotalCount++
			if info.Conditions.Ready {
				entry.ReadyCount++
			} else {
				entry.NotReadyCount++
			}
		}
	}

	return entry
}
