package argo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFillEventOccurrence(t *testing.T) {
	t.Parallel()

	legacyTime := metav1.NewTime(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	observedTime := metav1.NewMicroTime(time.Date(2026, 10, 2, 8, 30, 0, 0, time.UTC))
	eventTime := metav1.NewMicroTime(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))

	list := FillEventOccurrence(&corev1.EventList{
		Items: []corev1.Event{
			{
				ObjectMeta:     metav1.ObjectMeta{Name: "legacy"},
				Count:          5,
				FirstTimestamp: legacyTime,
				LastTimestamp:  legacyTime,
			},
			{
				ObjectMeta:    metav1.ObjectMeta{Name: "mixed"},
				Count:         5,
				LastTimestamp: legacyTime,
				Series:        &corev1.EventSeries{Count: 9, LastObservedTime: observedTime},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "singleton"},
				EventTime:  eventTime,
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "empty"},
			},
		},
	})

	require.NotNil(t, list)
	require.Len(t, list.Items, 4)

	legacy := list.Items[0]
	assert.Equal(t, int32(5), legacy.Count)
	assert.Equal(t, legacyTime, legacy.FirstTimestamp)
	assert.Equal(t, legacyTime, legacy.LastTimestamp)

	mixed := list.Items[1]
	assert.Equal(t, int32(9), mixed.Count)
	assert.Equal(t, metav1.NewTime(observedTime.Time), mixed.LastTimestamp)

	singleton := list.Items[2]
	assert.Equal(t, int32(1), singleton.Count)
	assert.Equal(t, metav1.NewTime(eventTime.Time), singleton.FirstTimestamp)
	assert.Equal(t, metav1.NewTime(eventTime.Time), singleton.LastTimestamp)

	empty := list.Items[3]
	assert.Equal(t, int32(0), empty.Count)
	assert.True(t, empty.FirstTimestamp.IsZero())
	assert.True(t, empty.LastTimestamp.IsZero())

	assert.Nil(t, FillEventOccurrence(nil))
}
