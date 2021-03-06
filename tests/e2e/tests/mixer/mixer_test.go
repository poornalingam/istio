// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package mixer defines integration tests that validate working mixer
// functionality in context of a test Istio-enabled cluster.
package mixer

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/api"
	"github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"istio.io/istio/devel/fortio"
	"istio.io/istio/tests/e2e/framework"
	"istio.io/istio/tests/e2e/util"
)

const (
	bookinfoYaml             = "samples/apps/bookinfo/bookinfo.yaml"
	rulesDir                 = "samples/apps/bookinfo/rules"
	rateLimitRule            = "mixer-rule-ratings-ratelimit.yaml"
	denialRule               = "mixer-rule-ratings-denial.yaml"
	newTelemetryRule         = "mixer-rule-additional-telemetry.yaml"
	routeAllRule             = "route-rule-all-v1.yaml"
	routeReviewsVersionsRule = "route-rule-reviews-v2-v3.yaml"
	routeReviewsV3Rule       = "route-rule-reviews-v3.yaml"
	emptyRule                = "mixer-rule-empty-rule.yaml"

	prometheusPort = "9090"

	targetLabel       = "target"
	responseCodeLabel = "response_code"

	global = "global"
)

type testConfig struct {
	*framework.CommonConfig
	gateway  string
	rulesDir string
}

var (
	tc             *testConfig
	testRetryTimes = 5
	rules          = []string{rateLimitRule, denialRule, newTelemetryRule, routeAllRule,
		routeReviewsVersionsRule, routeReviewsV3Rule, emptyRule}
)

func (t *testConfig) Setup() error {
	t.gateway = "http://" + tc.Kube.Ingress
	for _, rule := range rules {
		src := util.GetResourcePath(filepath.Join(rulesDir, rule))
		dest := filepath.Join(t.rulesDir, rule)
		srcBytes, err := ioutil.ReadFile(src)
		if err != nil {
			glog.Errorf("Failed to read original rule file %s", src)
			return err
		}
		err = ioutil.WriteFile(dest, srcBytes, 0600)
		if err != nil {
			glog.Errorf("Failed to write into new rule file %s", dest)
			return err
		}
	}
	return createDefaultRoutingRules()
}

func createDefaultRoutingRules() error {
	if err := createRouteRule(routeAllRule); err != nil {
		return fmt.Errorf("could not create base routing rules: %v", err)
	}
	allowRuleSync()
	return nil
}

func (t *testConfig) Teardown() error {
	return deleteDefaultRoutingRules()
}

func deleteDefaultRoutingRules() error {
	if err := deleteRouteRule(routeAllRule); err != nil {
		return fmt.Errorf("could not delete default routing rule: %v", err)
	}
	return nil
}

type promProxy struct {
	namespace      string
	portFwdProcess *os.Process
}

func newPromProxy(namespace string) *promProxy {
	return &promProxy{
		namespace: namespace,
	}
}

func (p *promProxy) Setup() error {
	var pod string
	var err error
	glog.Info("Setting up prometheus proxy")
	getName := fmt.Sprintf("kubectl -n %s get pod -l app=prometheus -o jsonpath='{.items[0].metadata.name}'", p.namespace)
	pod, err = util.Shell(getName)
	if err != nil {
		return err
	}
	glog.Info("prometheus pod name: ", pod)
	portFwdCmd := fmt.Sprintf("kubectl port-forward %s %s:%s -n %s", strings.Trim(pod, "'"), prometheusPort, prometheusPort, p.namespace)
	glog.Info(portFwdCmd)
	if p.portFwdProcess, err = util.RunBackground(portFwdCmd); err != nil {
		glog.Errorf("Failed to port forward: %s", err)
		return err
	}
	glog.Infof("running prometheus port-forward in background, pid = %d", p.portFwdProcess.Pid)
	return err
}

func (p *promProxy) Teardown() (err error) {
	glog.Info("Cleaning up mixer proxy")
	if p.portFwdProcess != nil {
		err := p.portFwdProcess.Kill()
		if err != nil {
			glog.Errorf("Failed to kill port-forward process, pid: %d", p.portFwdProcess.Pid)
		}
	}
	return
}
func TestMain(m *testing.M) {
	flag.Parse()
	check(framework.InitGlog(), "cannot setup glog")
	check(setTestConfig(), "could not create TestConfig")
	tc.Cleanup.RegisterCleanable(tc)
	os.Exit(tc.RunTest(m))
}

func TestGlobalCheckAndReport(t *testing.T) {
	if err := visitProductPage(testRetryTimes); err != nil {
		t.Fatalf("Test app setup failure: %v", err)
	}
	allowPrometheusSync()

	glog.Info("Successfully sent request(s) to /productpage; checking metrics...")

	promAPI, err := promAPI()
	if err != nil {
		t.Fatalf("Could not build prometheus API client: %v", err)
	}
	query := fmt.Sprintf("request_count{%s=\"%s\",%s=\"200\"}", targetLabel, fqdn("productpage"), responseCodeLabel)
	t.Logf("prometheus query: %s", query)
	value, err := promAPI.Query(context.Background(), query, time.Now())
	if err != nil {
		t.Fatalf("Could not get metrics from prometheus: %v", err)
	}
	glog.Infof("promvalue := %s", value.String())

	got, err := vectorValue(value, map[string]string{})
	if err != nil {
		t.Logf("prometheus values for request_count:\n%s", promDump(promAPI, "request_count"))
		t.Fatalf("Could not find metric value: %v", err)
	}
	want := float64(1)
	if got < want {
		t.Logf("prometheus values for request_count:\n%s", promDump(promAPI, "request_count"))
		t.Errorf("Bad metric value: got %f, want at least %f", got, want)
	}
}

func TestNewMetrics(t *testing.T) {
	productpage := fqdn("productpage")
	if err := createMixerRule(global, productpage, newTelemetryRule); err != nil {
		t.Fatalf("could not create required mixer rule: %v", err)
	}
	defer func() {
		if err := createMixerRule(global, productpage, emptyRule); err != nil {
			t.Logf("could not clear rule: %v", err)
		}
	}()

	allowRuleSync()

	if err := visitProductPage(testRetryTimes); err != nil {
		t.Fatalf("Test app setup failure: %v", err)
	}

	glog.Info("Successfully sent request(s) to /productpage; checking metrics...")
	allowPrometheusSync()

	promAPI, err := promAPI()
	if err != nil {
		t.Fatalf("Could not build prometheus API client: %v", err)
	}
	query := fmt.Sprintf("response_size_count{%s=\"%s\",%s=\"200\"}", targetLabel, fqdn("productpage"), responseCodeLabel)
	t.Logf("prometheus query: %s", query)
	value, err := promAPI.Query(context.Background(), query, time.Now())
	if err != nil {
		t.Fatalf("Could not get metrics from prometheus: %v", err)
	}
	glog.Infof("promvalue := %s", value.String())

	got, err := vectorValue(value, map[string]string{})
	if err != nil {
		t.Logf("prometheus values for response_size_count:\n%s", promDump(promAPI, "response_size_count"))
		t.Logf("prometheus values for request_count:\n%s", promDump(promAPI, "request_count"))
		t.Fatalf("Could not find metric value: %v", err)
	}
	want := float64(1)
	if got < want {
		t.Logf("prometheus values for response_size_count:\n%s", promDump(promAPI, "response_size_count"))
		t.Logf("prometheus values for request_count:\n%s", promDump(promAPI, "request_count"))
		t.Errorf("Bad metric value: got %f, want at least %f", got, want)
	}
}

func TestDenials(t *testing.T) {
	if err := replaceRouteRule(routeReviewsV3Rule); err != nil {
		t.Fatalf("Could not create replace reviews routing rule: %v", err)
	}
	ratings := fqdn("ratings")
	if err := createMixerRule(global, ratings, denialRule); err != nil {
		t.Fatalf("Could not create required mixer rule: %v", err)
	}
	defer func() {
		if err := createMixerRule(global, ratings, emptyRule); err != nil {
			t.Logf("could not clear rule: %v", err)
		}
	}()
	allowRuleSync()

	// generate several calls to the product page
	for i := 0; i < 10; i++ {
		if err := visitProductPage(testRetryTimes); err != nil {
			t.Fatalf("Test app setup failure: %v", err)
		}
	}

	glog.Info("Successfully sent request(s) to /productpage; checking metrics...")
	allowPrometheusSync()

	promAPI, err := promAPI()
	if err != nil {
		t.Fatalf("Could not build prometheus API client: %v", err)
	}
	query := fmt.Sprintf("request_count{%s=\"%s\",%s=\"400\"}", targetLabel, fqdn("ratings"), responseCodeLabel)
	t.Logf("prometheus query: %s", query)
	value, err := promAPI.Query(context.Background(), query, time.Now())
	if err != nil {
		t.Fatalf("Could not get metrics from prometheus: %v", err)
	}
	glog.Infof("promvalue := %s", value.String())

	got, err := vectorValue(value, map[string]string{})
	if err != nil {
		t.Logf("prometheus values for request_count:\n%s", promDump(promAPI, "request_count"))
		t.Fatalf("Could not find metric value: %v", err)
	}
	want := float64(1)
	if got < want {
		t.Logf("prometheus values for request_count:\n%s", promDump(promAPI, "request_count"))
		t.Errorf("Bad metric value: got %f, want at least %f", got, want)
	}
}

func TestRateLimit(t *testing.T) {
	if err := replaceRouteRule(routeReviewsV3Rule); err != nil {
		t.Fatalf("Could not create replace reviews routing rule: %v", err)
	}
	// the rate limit rule applies a max rate limit of 5 rps. Here we apply it
	// to the "ratings" service (without a selector -- meaning for all versions).
	ratings := fqdn("ratings")
	if err := createMixerRule(global, ratings, rateLimitRule); err != nil {
		t.Fatalf("Could not create required mixer rule: %got", err)
	}
	defer func() {
		if err := createMixerRule(global, ratings, emptyRule); err != nil {
			t.Logf("could not clear rule: %got", err)
		}
	}()

	allowRuleSync()

	url := fmt.Sprintf("%s/productpage", tc.gateway)

	// run at a large QPS (here 100) for a minute to ensure that enough
	// traffic is generated to trigger 429s from the rate limit rule
	opts := fortio.HTTPRunnerOptions{
		RunnerOptions: fortio.RunnerOptions{
			QPS:        100,
			Duration:   1 * time.Minute,
			NumThreads: 8,
		},
		URL: url,
	}

	// productpage should still return 200s when ratings is rate-limited.
	res, err := fortio.RunHTTPTest(&opts)
	if err != nil {
		t.Fatalf("Generating traffic via fortio failed: %v", err)
	}

	allowPrometheusSync()

	// setup prometheus API
	promAPI, err := promAPI()
	if err != nil {
		t.Fatalf("Could not build prometheus API client: %v", err)
	}

	totalReqs := res.DurationHistogram.Count
	succReqs := res.RetCodes[http.StatusOK]
	badReqs := res.RetCodes[http.StatusBadRequest]

	glog.Info("Successfully sent request(s) to /productpage; checking metrics...")
	glog.Infof("Summary: %d reqs (%d 200s, %d 400s)", totalReqs, succReqs, badReqs)

	// consider only successful requests (as recorded at productpage service)
	callsToRatings := float64(succReqs)

	// the rate-limit is 5 rps, but observed actuals are [4-5) in experimental
	// testing. opt for leniency here to decrease flakiness of testing.
	want200s := opts.Duration.Seconds() * 4

	// everything in excess of 200s should be 429s (ideally)
	want429s := callsToRatings - want200s

	// if we received less traffic than the expected enforced limit to ratings
	// then there is no way to determine if the rate limit was applied at all
	// and for how much traffic. log all metrics and abort test.
	if callsToRatings < want200s {
		t.Logf("full set of prometheus metrics:\n%s", promDump(promAPI, "request_count"))
		t.Fatalf("Not enough traffic generated to exercise rate limit: ratings_reqs=%f, want200s=%f", callsToRatings, want200s)
	}

	query := fmt.Sprintf("request_count{%s=\"%s\"}", targetLabel, fqdn("ratings"))
	t.Logf("prometheus query: %s", query)
	value, err := promAPI.Query(context.Background(), query, time.Now())
	if err != nil {
		t.Fatalf("Could not get metrics from prometheus: %v", err)
	}
	glog.Infof("promvalue := %s", value.String())

	got, err := vectorValue(value, map[string]string{responseCodeLabel: "429"})
	if err != nil {
		t.Logf("prometheus values for request_count:\n%s", promDump(promAPI, "request_count"))
		t.Fatalf("Could not find rate limit value: %v", err)
	}

	// establish some baseline to protect against flakiness due to randomness in routing
	want := math.Floor(want429s * .75)

	// check resource exhausteds
	if got < want {
		t.Errorf("Bad metric value for rate-limited requests (429s): got %f, want at least %f", got, want)
	}

	got, err = vectorValue(value, map[string]string{responseCodeLabel: "200"})
	if err != nil {
		t.Logf("prometheus values for request_count:\n%s", promDump(promAPI, "request_count"))
		t.Fatalf("Could not find successes value: %v", err)
	}

	// establish some baseline to protect against flakiness due to randomness in routing
	want = math.Floor(want200s * .75)

	// check successes
	if got < want {
		t.Logf("prometheus values for request_count:\n%s", promDump(promAPI, "request_count"))
		t.Errorf("Bad metric value for successful requests (200s): got %f, want at least %f", got, want)
	}
}

func allowRuleSync() {
	glog.Info("Sleeping to allow rules to take effect...")
	time.Sleep(1 * time.Minute)
}

func allowPrometheusSync() {
	glog.Info("Sleeping to allow prometheus to record metrics...")
	time.Sleep(30 * time.Second)
}

func promAPI() (v1.API, error) {
	client, err := api.NewClient(api.Config{Address: fmt.Sprintf("http://localhost:%s", prometheusPort)})
	if err != nil {
		return nil, err
	}
	return v1.NewAPI(client), nil
}

// promDump gets all of the recorded values for a metric by name and generates a report of the values.
// used for debugging of failures to provide a comprehensive view of traffic experienced.
func promDump(client v1.API, metric string) string {
	if value, err := client.Query(context.Background(), fmt.Sprintf("%s{}", metric), time.Now()); err == nil {
		return value.String()
	}
	return ""
}

func vectorValue(val model.Value, labels map[string]string) (float64, error) {
	if val.Type() != model.ValVector {
		return 0, fmt.Errorf("value not a model.Vector; was %s", val.Type().String())
	}

	value := val.(model.Vector)
	for _, sample := range value {
		metric := sample.Metric
		nameCount := len(labels)
		for k, v := range metric {
			if labelVal, ok := labels[string(k)]; ok && labelVal == string(v) {
				nameCount--
			}
		}
		if nameCount == 0 {
			return float64(sample.Value), nil
		}
	}
	return 0, fmt.Errorf("value not found for %#v", labels)
}

func visitProductPage(retries int) error {
	standby := 0
	for i := 0; i <= retries; i++ {
		time.Sleep(time.Duration(standby) * time.Second)
		http.DefaultClient.Timeout = 1 * time.Minute
		resp, err := http.Get(fmt.Sprintf("%s/productpage", tc.gateway))
		if err != nil {
			glog.Infof("Error talking to productpage: %s", err)
		} else {
			glog.Infof("Get from page: %d", resp.StatusCode)
			if resp.StatusCode == http.StatusOK {
				glog.Info("Get response from product page!")
				break
			}
			closeResponseBody(resp)
		}
		if i == retries {
			return errors.New("could not retrieve product page: default route Failed")
		}
		standby += 5
		glog.Errorf("Couldn't get to the bookinfo product page, trying again in %d second", standby)
	}
	if standby != 0 {
		time.Sleep(time.Minute)
	}
	return nil
}

func fqdn(service string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", service, tc.Kube.Namespace)
}

func createRouteRule(ruleName string) error {
	rule := filepath.Join(tc.rulesDir, ruleName)
	return util.KubeApply(tc.Kube.Namespace, rule)
}

func replaceRouteRule(ruleName string) error {
	rule := filepath.Join(tc.rulesDir, ruleName)
	return util.KubeApply(tc.Kube.Namespace, rule)
}

func deleteRouteRule(ruleName string) error {
	rule := filepath.Join(tc.rulesDir, ruleName)
	return util.KubeDelete(tc.Kube.Namespace, rule)
}

func createMixerRule(scope, subject, ruleName string) error {
	rule := filepath.Join(tc.rulesDir, ruleName)
	return tc.Kube.Istioctl.CreateMixerRule(scope, subject, rule)
}

func setTestConfig() error {
	cc, err := framework.NewCommonConfig("mixer_test")
	if err != nil {
		return err
	}
	tc = new(testConfig)
	tc.CommonConfig = cc
	tmpDir, err := ioutil.TempDir(os.TempDir(), "mixer_test")
	if err != nil {
		return err
	}
	tc.rulesDir = tmpDir
	demoApp := &framework.App{
		AppYaml:    util.GetResourcePath(bookinfoYaml),
		KubeInject: true,
	}
	tc.Kube.AppManager.AddApp(demoApp)
	mp := newPromProxy(tc.Kube.Namespace)
	tc.Cleanup.RegisterCleanable(mp)
	return nil
}

func check(err error, msg string) {
	if err != nil {
		glog.Fatalf("%s. Error %s", msg, err)
	}
}

func closeResponseBody(r *http.Response) {
	if err := r.Body.Close(); err != nil {
		glog.Error(err)
	}
}
