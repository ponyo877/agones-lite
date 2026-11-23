package fleetautoscalers

import (
	"errors"
	"net/url"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
)

// Webhook sends a review to the URL of a Webhook policy and returns the
// answer. It is the one side effect a policy can have, so the caller
// supplies it.
//
// config.URL is set and parses as a URL when Webhook is called. The request
// has no UID yet: a UID names one round trip, so the one who makes the trip
// chooses it.
//
// Upstream: the HTTP half of applyWebhookPolicy, which posts the review as
// JSON and takes any status but 200 for an error.
type Webhook func(config *autoscalingv1.URLConfiguration, review *autoscalingv1.FleetAutoscaleReview) (*autoscalingv1.FleetAutoscaleReview, error)

// webhook lets an external service decide. A service that does not want to
// scale leaves the Fleet at the size its spec asks for.
//
// Upstream: applyWebhookPolicy.
func (p pass) webhook(config *autoscalingv1.URLConfiguration) (int32, bool, error) {
	if config == nil {
		return 0, false, errors.New("webhookPolicy parameter must not be nil")
	}
	if err := checkTarget(config); err != nil {
		return 0, false, err
	}
	if p.call == nil {
		return 0, false, errors.New("no Webhook was given to call")
	}
	answer, err := p.call(config, reviewOf(p.fleet))
	if err != nil {
		return 0, false, err
	}
	if answer == nil || answer.Response == nil {
		return 0, false, errors.New("webhook response missing required 'response' field")
	}
	if !answer.Response.Scale {
		return p.fleet.Spec.Replicas, false, nil
	}
	return answer.Response.Replicas, false, nil
}

// checkTarget makes sure the policy says where to send the review.
//
// Upstream: buildURLFromConfiguration. It also accepts a reference to a
// Kubernetes Service, and there are no Services here.
func checkTarget(config *autoscalingv1.URLConfiguration) error {
	switch {
	case config.URL != nil && config.Service != nil:
		return errors.New("service and URL cannot be used simultaneously")
	case config.Service != nil:
		return errors.New("webhook service is not supported, as there are no Kubernetes Services here: use url")
	case config.URL == nil || *config.URL == "":
		return errors.New("URL was not provided")
	}
	_, err := url.ParseRequestURI(*config.URL)
	return err
}

// reviewOf describes the Fleet to the webhook.
func reviewOf(fleet *agonesv1.Fleet) *autoscalingv1.FleetAutoscaleReview {
	return &autoscalingv1.FleetAutoscaleReview{
		Request: &autoscalingv1.FleetAutoscaleRequest{
			Name:      fleet.Name,
			Namespace: fleet.Namespace,
			Status:    fleet.Status,
			// Upstream's FleetAutoscaleRequestMetaData gate, on by default.
			Labels:      fleet.Labels,
			Annotations: fleet.Annotations,
		},
	}
}
