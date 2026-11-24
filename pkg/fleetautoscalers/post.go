package fleetautoscalers

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
)

// webhookTimeout is how long a webhook gets to answer. Same as upstream.
const webhookTimeout = 15 * time.Second

// Post is the Webhook that really asks: it sends the review to the policy's
// URL as JSON and reads the answer. It is the one place in this package
// with a side effect.
//
// Upstream: the HTTP half of applyWebhookPolicy in fleetautoscalers.go.
func Post(config *autoscalingv1.URLConfiguration, review *autoscalingv1.FleetAutoscaleReview) (*autoscalingv1.FleetAutoscaleReview, error) {
	if config == nil || config.URL == nil {
		return nil, errors.New("the webhook policy has no url")
	}
	client, err := webhookClient(config.CABundle)
	if err != nil {
		return nil, err
	}
	review.Request.UID = uuid.NewUUID() // lets the webhook tell requests apart
	body, err := json.Marshal(review)
	if err != nil {
		return nil, err
	}
	resp, err := client.Post(*config.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status code %d from the server: %s", resp.StatusCode, *config.URL)
	}
	answer := &autoscalingv1.FleetAutoscaleReview{}
	return answer, json.NewDecoder(resp.Body).Decode(answer)
}

// webhookClient trusts the CA bundle of the policy, or the system's CAs
// when there is none.
func webhookClient(caBundle []byte) (*http.Client, error) {
	client := &http.Client{Timeout: webhookTimeout}
	if len(caBundle) == 0 {
		return client, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundle) {
		return nil, errors.New("no certs were appended from caBundle")
	}
	client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return client, nil
}
