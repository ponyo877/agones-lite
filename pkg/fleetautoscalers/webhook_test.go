package fleetautoscalers

import (
	"errors"
	"strconv"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admregv1 "k8s.io/api/admissionregistration/v1"
)

const scaleURL = "http://autoscaler.example/scale"

// doubling is the webhook of upstream's tests (testServer): it doubles a
// Fleet of which more than 70% is Allocated, and otherwise leaves it alone.
// A fixedReplicas annotation on the Fleet overrides both.
func doubling(_ *autoscalingv1.URLConfiguration, review *autoscalingv1.FleetAutoscaleReview) (*autoscalingv1.FleetAutoscaleReview, error) {
	status := review.Request.Status
	response := &autoscalingv1.FleetAutoscaleResponse{Replicas: status.Replicas}
	if float32(status.AllocatedReplicas)/float32(status.Replicas) > 0.7 {
		response.Scale = true
		response.Replicas = status.Replicas * 2
	}
	if fixed, ok := review.Request.Annotations["fixedReplicas"]; ok {
		replicas, _ := strconv.Atoi(fixed)
		response.Scale = true
		response.Replicas = int32(replicas)
	}
	return &autoscalingv1.FleetAutoscaleReview{Request: review.Request, Response: response}, nil
}

// answering is a webhook that always gives the same answer.
func answering(review *autoscalingv1.FleetAutoscaleReview, err error) Webhook {
	return func(*autoscalingv1.URLConfiguration, *autoscalingv1.FleetAutoscaleReview) (*autoscalingv1.FleetAutoscaleReview, error) {
		return review, err
	}
}

// Upstream: TestApplyWebhookPolicy and TestApplyWebhookPolicyWithMetadata.
func TestWebhookDecidesTheSize(t *testing.T) {
	tests := map[string]struct {
		fleet *agonesv1.Fleet
		want  int32
	}{
		"an answer without scale leaves the Fleet as it is": {
			fleet: fleetOf(sized(50, 50, 10)),
			want:  50,
		},
		"as it is means the size the spec asks for, also in the middle of a scale down": {
			fleet: fleetOf(sized(10, 20, 14)),
			want:  10,
		},
		"70% Allocated is not more than 70%": {
			fleet: fleetOf(sized(50, 50, 35)),
			want:  50,
		},
		"an answer with scale is taken as it comes": {
			fleet: fleetOf(sized(50, 50, 40)),
			want:  100,
		},
		"the webhook sees the Fleet's annotations": {
			fleet: fleetOf(func(f *agonesv1.Fleet) { f.Annotations = map[string]string{"fixedReplicas": "11"} }),
			want:  11,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			replicas, limited, applied, err := Desired(webhookPolicy(scaleURL), tc.fleet, nil, nil, noon, doubling)

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas)
			assert.False(t, limited, "a webhook has no limits of its own")
			assert.Equal(t, webhookType, applied)
		})
	}
}

func TestWebhookIsToldAboutTheFleet(t *testing.T) {
	fleet := fleetOf(func(f *agonesv1.Fleet) {
		f.Labels = map[string]string{"game": "chess"}
		f.Annotations = map[string]string{"owner": "ops"}
	})
	policy := webhookPolicy(scaleURL)
	policy.Webhook.CABundle = []byte("a certificate")
	var config *autoscalingv1.URLConfiguration
	var review *autoscalingv1.FleetAutoscaleReview
	record := func(c *autoscalingv1.URLConfiguration, r *autoscalingv1.FleetAutoscaleReview) (*autoscalingv1.FleetAutoscaleReview, error) {
		config, review = c, r
		return doubling(c, r)
	}

	_, _, _, err := Desired(policy, fleet, nil, nil, noon, record)

	require.NoError(t, err)
	assert.Same(t, policy.Webhook, config, "the caller needs the URL and the CA bundle")
	assert.Equal(t, &autoscalingv1.FleetAutoscaleRequest{
		Name:        "fleet-1",
		Namespace:   "default",
		Status:      fleet.Status,
		Labels:      map[string]string{"game": "chess"},
		Annotations: map[string]string{"owner": "ops"},
	}, review.Request, "the UID is left for the caller to choose")
	assert.Nil(t, review.Response)
}

func TestWebhookThatDoesNotAnswerProperlyIsAnError(t *testing.T) {
	tests := map[string]struct {
		call Webhook
		want string
	}{
		"the call fails": {
			call: answering(nil, errors.New("bad status code 500 from the server")),
			want: "bad status code 500 from the server",
		},
		// Upstream issue #4555: an answer of {} used to panic.
		"the answer has no response in it": {
			call: answering(&autoscalingv1.FleetAutoscaleReview{}, nil),
			want: "webhook response missing required 'response' field",
		},
		"there is no answer at all": {
			call: answering(nil, nil),
			want: "webhook response missing required 'response' field",
		},
		// Upstream passes it on, and its API server refuses the Fleet.
		"the answer is a size below zero": {
			call: answering(&autoscalingv1.FleetAutoscaleReview{
				Response: &autoscalingv1.FleetAutoscaleResponse{Scale: true, Replicas: -5},
			}, nil),
			want: "webhook response asks for -5 replicas, which is fewer than none",
		},
		"there is nothing to call": {
			call: nil,
			want: "no Webhook was given to call",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := Desired(webhookPolicy(scaleURL), fleetOf(), nil, nil, noon, tc.call)

			assert.EqualError(t, err, tc.want)
		})
	}
}

// Upstream: the cases of TestApplyWebhookPolicy that never reach the server.
func TestWebhookWithoutAUsableURLIsNotCalled(t *testing.T) {
	url, empty, invalid := scaleURL, "", ")1golang.org/"
	service := &admregv1.ServiceReference{Namespace: "default", Name: "service1"}
	tests := map[string]struct {
		webhook *autoscalingv1.URLConfiguration
		want    string
	}{
		"no webhook parameters at all": {
			webhook: nil,
			want:    "webhookPolicy parameter must not be nil",
		},
		"a URL and a Service together": {
			webhook: &autoscalingv1.URLConfiguration{URL: &url, Service: service},
			want:    "service and URL cannot be used simultaneously",
		},
		"a Service, which upstream would look up in Kubernetes": {
			webhook: &autoscalingv1.URLConfiguration{Service: service},
			want:    "webhook service is not supported, as there are no Kubernetes Services here: use url",
		},
		"neither a URL nor a Service": {
			webhook: &autoscalingv1.URLConfiguration{},
			want:    "URL was not provided",
		},
		"an empty URL": {
			webhook: &autoscalingv1.URLConfiguration{URL: &empty},
			want:    "URL was not provided",
		},
		"a URL that does not parse": {
			webhook: &autoscalingv1.URLConfiguration{URL: &invalid},
			want:    `parse ")1golang.org/": invalid URI for request`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			policy := autoscalingv1.FleetAutoscalerPolicy{Type: webhookType, Webhook: tc.webhook}

			_, _, _, err := Desired(policy, fleetOf(), nil, nil, noon, neverCalled(t))

			assert.EqualError(t, err, tc.want)
		})
	}
}
