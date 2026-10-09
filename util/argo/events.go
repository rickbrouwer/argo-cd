package argo

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FillEventOccurrence fills the legacy count and timestamp fields for events created
// through events.k8s.io/v1 the way kubectl does: series wins when present, singletons
// count once, and timestamps fall back to eventTime.
func FillEventOccurrence(list *corev1.EventList) *corev1.EventList {
	if list == nil {
		return nil
	}
	for i := range list.Items {
		e := &list.Items[i]
		if e.Series != nil {
			e.Count = e.Series.Count
			if !e.Series.LastObservedTime.IsZero() {
				e.LastTimestamp = metav1.NewTime(e.Series.LastObservedTime.Time)
			}
		} else if e.Count == 0 && !e.EventTime.IsZero() {
			e.Count = 1
		}
		if e.FirstTimestamp.IsZero() && !e.EventTime.IsZero() {
			e.FirstTimestamp = metav1.NewTime(e.EventTime.Time)
		}
		if e.LastTimestamp.IsZero() && !e.EventTime.IsZero() {
			e.LastTimestamp = metav1.NewTime(e.EventTime.Time)
		}
	}
	return list
}
