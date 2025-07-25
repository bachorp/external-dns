/*
Copyright 2017 The Kubernetes Authors.

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

package source

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"

	log "github.com/sirupsen/logrus"
	networkv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	kubeinformers "k8s.io/client-go/informers"
	netinformers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"sigs.k8s.io/external-dns/source/informers"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/source/annotations"
	"sigs.k8s.io/external-dns/source/fqdn"
)

const (
	// Possible values for the ingress-hostname-source annotation
	IngressHostnameSourceAnnotationOnlyValue   = "annotation-only"
	IngressHostnameSourceDefinedHostsOnlyValue = "defined-hosts-only"

	IngressClassAnnotationKey = "kubernetes.io/ingress.class"
)

// ingressSource is an implementation of Source for Kubernetes ingress objects.
// Ingress implementation will use the spec.rules.host value for the hostname
// Use targetAnnotationKey to explicitly set Endpoint. (useful if the ingress
// controller does not update, or to override with alternative endpoint)
type ingressSource struct {
	client                   kubernetes.Interface
	namespace                string
	annotationFilter         string
	ingressClassNames        []string
	fqdnTemplate             *template.Template
	combineFQDNAnnotation    bool
	ignoreHostnameAnnotation bool
	ingressInformer          netinformers.IngressInformer
	ignoreIngressTLSSpec     bool
	ignoreIngressRulesSpec   bool
	labelSelector            labels.Selector
}

// NewIngressSource creates a new ingressSource with the given config.
func NewIngressSource(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace, annotationFilter, fqdnTemplate string,
	combineFqdnAnnotation, ignoreHostnameAnnotation, ignoreIngressTLSSpec, ignoreIngressRulesSpec bool,
	labelSelector labels.Selector,
	ingressClassNames []string) (Source, error) {
	tmpl, err := fqdn.ParseTemplate(fqdnTemplate)
	if err != nil {
		return nil, err
	}

	// ensure that ingress class is only set in either the ingressClassNames or
	// annotationFilter but not both
	if ingressClassNames != nil && annotationFilter != "" {
		selector, err := getLabelSelector(annotationFilter)
		if err != nil {
			return nil, err
		}

		requirements, _ := selector.Requirements()
		for _, requirement := range requirements {
			if requirement.Key() == "kubernetes.io/ingress.class" {
				return nil, errors.New("--ingress-class is mutually exclusive with the kubernetes.io/ingress.class annotation filter")
			}
		}
	}
	// Use shared informer to listen for add/update/delete of ingresses in the specified namespace.
	// Set resync period to 0, to prevent processing when nothing has changed.
	informerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, 0, kubeinformers.WithNamespace(namespace))
	ingressInformer := informerFactory.Networking().V1().Ingresses()

	// Add default resource event handlers to properly initialize informer.
	ingressInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
			},
		},
	)

	informerFactory.Start(ctx.Done())

	// wait for the local cache to be populated.
	if err := informers.WaitForCacheSync(context.Background(), informerFactory); err != nil {
		return nil, err
	}

	sc := &ingressSource{
		client:                   kubeClient,
		namespace:                namespace,
		annotationFilter:         annotationFilter,
		ingressClassNames:        ingressClassNames,
		fqdnTemplate:             tmpl,
		combineFQDNAnnotation:    combineFqdnAnnotation,
		ignoreHostnameAnnotation: ignoreHostnameAnnotation,
		ingressInformer:          ingressInformer,
		ignoreIngressTLSSpec:     ignoreIngressTLSSpec,
		ignoreIngressRulesSpec:   ignoreIngressRulesSpec,
		labelSelector:            labelSelector,
	}
	return sc, nil
}

// Endpoints returns endpoint objects for each host-target combination that should be processed.
// Retrieves all ingress resources on all namespaces
func (sc *ingressSource) Endpoints(_ context.Context) ([]*endpoint.Endpoint, error) {
	ingresses, err := sc.ingressInformer.Lister().Ingresses(sc.namespace).List(sc.labelSelector)
	if err != nil {
		return nil, err
	}
	ingresses, err = sc.filterByAnnotations(ingresses)
	if err != nil {
		return nil, err
	}

	ingresses, err = sc.filterByIngressClass(ingresses)
	if err != nil {
		return nil, err
	}

	endpoints := []*endpoint.Endpoint{}

	for _, ing := range ingresses {
		// Check the controller annotation to see if we are responsible.
		if controller, ok := ing.Annotations[controllerAnnotationKey]; ok && controller != controllerAnnotationValue {
			log.Debugf("Skipping ingress %s/%s because controller value does not match, found: %s, required: %s",
				ing.Namespace, ing.Name, controller, controllerAnnotationValue)
			continue
		}

		ingEndpoints := endpointsFromIngress(ing, sc.ignoreHostnameAnnotation, sc.ignoreIngressTLSSpec, sc.ignoreIngressRulesSpec)

		// apply template if host is missing on ingress
		if (sc.combineFQDNAnnotation || len(ingEndpoints) == 0) && sc.fqdnTemplate != nil {
			iEndpoints, err := sc.endpointsFromTemplate(ing)
			if err != nil {
				return nil, err
			}

			ingEndpoints = append(ingEndpoints, iEndpoints...)
		}

		if len(ingEndpoints) == 0 {
			log.Debugf("No endpoints could be generated from ingress %s/%s", ing.Namespace, ing.Name)
			continue
		}

		log.Debugf("Endpoints generated from ingress: %s/%s: %v", ing.Namespace, ing.Name, ingEndpoints)
		endpoints = append(endpoints, ingEndpoints...)
	}

	for _, ep := range endpoints {
		sort.Sort(ep.Targets)
	}

	return endpoints, nil
}

func (sc *ingressSource) endpointsFromTemplate(ing *networkv1.Ingress) ([]*endpoint.Endpoint, error) {
	hostnames, err := fqdn.ExecTemplate(sc.fqdnTemplate, ing)
	if err != nil {
		return nil, err
	}

	resource := fmt.Sprintf("ingress/%s/%s", ing.Namespace, ing.Name)

	ttl := annotations.TTLFromAnnotations(ing.Annotations, resource)

	targets := annotations.TargetsFromTargetAnnotation(ing.Annotations)
	if len(targets) == 0 {
		targets = targetsFromIngressStatus(ing.Status)
	}

	providerSpecific, setIdentifier := annotations.ProviderSpecificAnnotations(ing.Annotations)

	var endpoints []*endpoint.Endpoint
	for _, hostname := range hostnames {
		endpoints = append(endpoints, EndpointsForHostname(hostname, targets, ttl, providerSpecific, setIdentifier, resource)...)
	}
	return endpoints, nil
}

// filterByAnnotations filters a list of ingresses by a given annotation selector.
func (sc *ingressSource) filterByAnnotations(ingresses []*networkv1.Ingress) ([]*networkv1.Ingress, error) {
	selector, err := getLabelSelector(sc.annotationFilter)
	if err != nil {
		return nil, err
	}

	// empty filter returns original list
	if selector.Empty() {
		return ingresses, nil
	}

	filteredList := []*networkv1.Ingress{}

	for _, ingress := range ingresses {
		// include ingress if its annotations match the selector
		if matchLabelSelector(selector, ingress.Annotations) {
			filteredList = append(filteredList, ingress)
		}
	}

	return filteredList, nil
}

// filterByIngressClass filters a list of ingresses based on a required ingress
// class
func (sc *ingressSource) filterByIngressClass(ingresses []*networkv1.Ingress) ([]*networkv1.Ingress, error) {
	// if no class filter is specified then there's nothing to do
	if len(sc.ingressClassNames) == 0 {
		return ingresses, nil
	}

	classNameReq, err := labels.NewRequirement(IngressClassAnnotationKey, selection.In, sc.ingressClassNames)
	if err != nil {
		return nil, err
	}

	selector := labels.NewSelector()
	selector = selector.Add(*classNameReq)

	filteredList := []*networkv1.Ingress{}

	for _, ingress := range ingresses {
		var matched = false

		for _, nameFilter := range sc.ingressClassNames {
			if ingress.Spec.IngressClassName != nil && len(*ingress.Spec.IngressClassName) > 0 {
				if nameFilter == *ingress.Spec.IngressClassName {
					matched = true
				}
			} else if matchLabelSelector(selector, ingress.Annotations) {
				matched = true
			}

			if matched {
				filteredList = append(filteredList, ingress)
				break
			}
		}

		if !matched {
			log.Debugf("Discarding ingress %s/%s because it does not match required ingress classes %v", ingress.Namespace, ingress.Name, sc.ingressClassNames)
		}
	}

	return filteredList, nil
}

// endpointsFromIngress extracts the endpoints from ingress object
func endpointsFromIngress(ing *networkv1.Ingress, ignoreHostnameAnnotation bool, ignoreIngressTLSSpec bool, ignoreIngressRulesSpec bool) []*endpoint.Endpoint {
	resource := fmt.Sprintf("ingress/%s/%s", ing.Namespace, ing.Name)

	ttl := annotations.TTLFromAnnotations(ing.Annotations, resource)

	targets := annotations.TargetsFromTargetAnnotation(ing.Annotations)

	if len(targets) == 0 {
		targets = targetsFromIngressStatus(ing.Status)
	}

	providerSpecific, setIdentifier := annotations.ProviderSpecificAnnotations(ing.Annotations)

	// Gather endpoints defined on hosts sections of the ingress
	var definedHostsEndpoints []*endpoint.Endpoint
	// Skip endpoints if we do not want entries from Rules section
	if !ignoreIngressRulesSpec {
		for _, rule := range ing.Spec.Rules {
			if rule.Host == "" {
				continue
			}
			definedHostsEndpoints = append(definedHostsEndpoints, EndpointsForHostname(rule.Host, targets, ttl, providerSpecific, setIdentifier, resource)...)
		}
	}

	// Skip endpoints if we do not want entries from tls spec section
	if !ignoreIngressTLSSpec {
		for _, tls := range ing.Spec.TLS {
			for _, host := range tls.Hosts {
				if host == "" {
					continue
				}
				definedHostsEndpoints = append(definedHostsEndpoints, EndpointsForHostname(host, targets, ttl, providerSpecific, setIdentifier, resource)...)
			}
		}
	}

	// Gather endpoints defined on annotations in the ingress
	var annotationEndpoints []*endpoint.Endpoint
	if !ignoreHostnameAnnotation {
		for _, hostname := range annotations.HostnamesFromAnnotations(ing.Annotations) {
			annotationEndpoints = append(annotationEndpoints, EndpointsForHostname(hostname, targets, ttl, providerSpecific, setIdentifier, resource)...)
		}
	}

	// Determine which hostnames to consider in our final list
	hostnameSourceAnnotation, hostnameSourceAnnotationExists := ing.Annotations[ingressHostnameSourceKey]
	if !hostnameSourceAnnotationExists {
		return append(definedHostsEndpoints, annotationEndpoints...)
	}

	// Include endpoints according to the hostname source annotation in our final list
	var endpoints []*endpoint.Endpoint
	if strings.ToLower(hostnameSourceAnnotation) == IngressHostnameSourceDefinedHostsOnlyValue {
		endpoints = append(endpoints, definedHostsEndpoints...)
	}
	if strings.ToLower(hostnameSourceAnnotation) == IngressHostnameSourceAnnotationOnlyValue {
		endpoints = append(endpoints, annotationEndpoints...)
	}
	return endpoints
}

func targetsFromIngressStatus(status networkv1.IngressStatus) endpoint.Targets {
	var targets endpoint.Targets

	for _, lb := range status.LoadBalancer.Ingress {
		if lb.IP != "" {
			targets = append(targets, lb.IP)
		}
		if lb.Hostname != "" {
			targets = append(targets, lb.Hostname)
		}
	}

	return targets
}

func (sc *ingressSource) AddEventHandler(ctx context.Context, handler func()) {
	log.Debug("Adding event handler for ingress")

	// Right now there is no way to remove event handler from informer, see:
	// https://github.com/kubernetes/kubernetes/issues/79610
	sc.ingressInformer.Informer().AddEventHandler(eventHandlerFunc(handler))
}
