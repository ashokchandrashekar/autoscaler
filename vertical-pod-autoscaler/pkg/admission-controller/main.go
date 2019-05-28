/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"net/http"
	"os"
	"time"

	"github.com/golang/glog"
	kube_flag "k8s.io/apiserver/pkg/util/flag"
	"k8s.io/autoscaler/vertical-pod-autoscaler/common"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/admission-controller/logic"
	vpa_clientset "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/clientset/versioned"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/target"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/metrics"
	metrics_admission "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/metrics/admission"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
	"k8s.io/client-go/informers"
	kube_client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	defaultResyncPeriod time.Duration = 10 * time.Minute
)

var (
	certsConfiguration = &certsConfig{
		clientCaFile:  flag.String("client-ca-file", "/etc/tls-certs/caCert.pem", "Path to CA PEM file."),
		tlsCertFile:   flag.String("tls-cert-file", "/etc/tls-certs/serverCert.pem", "Path to server certificate PEM file."),
		tlsPrivateKey: flag.String("tls-private-key", "/etc/tls-certs/serverKey.pem", "Path to server certificate key PEM file."),
	}

	address             = flag.String("address", ":8944", "The address to expose Prometheus metrics.")
	allowToAdjustLimits = flag.Bool("allow-to-adjust-limits", false, "If set to true, admission webhook will set limits per container too if needed")
	namespace           = os.Getenv("NAMESPACE")
)

func main() {
	kube_flag.InitFlags()
	glog.V(1).Infof("Vertical Pod Autoscaler %s Admission Controller", common.VerticalPodAutoscalerVersion)

	healthCheck := metrics.NewHealthCheck(time.Minute, false)
	metrics.Initialize(*address, healthCheck)
	metrics_admission.Register()

	certs := initCerts(*certsConfiguration)

	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Fatal(err)
	}

	vpaClient := vpa_clientset.NewForConfigOrDie(config)
	vpaLister := vpa_api_util.NewAllVpasLister(vpaClient, make(chan struct{}))
	kubeClient := kube_client.NewForConfigOrDie(config)
	factory := informers.NewSharedInformerFactory(kubeClient, defaultResyncPeriod)
	targetSelectorFetcher := target.NewCompositeTargetSelectorFetcher(
		target.NewVpaTargetSelectorFetcher(config, kubeClient, factory),
		target.NewBeta1TargetSelectorFetcher(config),
	)
	podPreprocessor := logic.NewDefaultPodPreProcessor()
	var limitsChecker logic.LimitRangeCalculator
	limitsChecker, err = logic.NewLimitsRangeCalculator(factory)
	if err != nil {
		klog.Errorf("Failed to create limitsChecker, falling back to not checking limits. Error message: %s", err)
		limitsChecker = logic.NewNoopLimitsCalculator()
	}
	recommendationProvider := logic.NewRecommendationProvider(limitsChecker, vpa_api_util.NewCappingRecommendationProcessor(), targetSelectorFetcher, vpaLister)

	as := logic.NewAdmissionServer(recommendationProvider, podPreprocessor, limitsChecker)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		as.Serve(w, r)
		healthCheck.UpdateLastActivity()
	})
	clientset := getClient()
	server := &http.Server{
		Addr:      ":8000",
		TLSConfig: configTLS(clientset, certs.serverCert, certs.serverKey),
	}
	go selfRegistration(clientset, certs.caCert, &namespace)
	server.ListenAndServeTLS("", "")
}
