/*
Copyright 2017 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sparkapplication

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"

	"github.com/golang/glog"

	apiv1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/apis/sparkoperator.k8s.io/v1beta2"
	"github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/config"
)

const (
	sparkUIPortConfigurationKey        = "spark.ui.port"
	defaultSparkWebUIPort       int32  = 4040
	defaultSparkWebUIPortName   string = "spark-driver-ui-port"
)

var ingressAppNameURLRegex = regexp.MustCompile("{{\\s*[$]appName\\s*}}")
var ingressAppNamespaceURLRegex = regexp.MustCompile("{{\\s*[$]appNamespace\\s*}}")

func getSparkUIingressURL(ingressURLFormat string, appName string, appNamespace string) (*url.URL, error) {
	ingressURL := ingressAppNamespaceURLRegex.ReplaceAllString(ingressAppNameURLRegex.ReplaceAllString(ingressURLFormat, appName), appNamespace)
	parsedURL, err := url.Parse(ingressURL)
	if err != nil {
		return nil, err
	}
	if parsedURL.Scheme == "" {
		//url does not contain any scheme, adding http:// so url.Parse can function correctly
		parsedURL, err = url.Parse("http://" + ingressURL)
		if err != nil {
			return nil, err
		}
	}
	return parsedURL, nil
}

// SparkService encapsulates information about the driver UI service.
type SparkService struct {
	serviceName        string
	serviceType        apiv1.ServiceType
	servicePort        int32
	servicePortName    string
	targetPort         intstr.IntOrString
	serviceIP          string
	serviceAnnotations map[string]string
}

// SparkIngress encapsulates information about the driver UI ingress.
type SparkIngress struct {
	ingressName string
	ingressURL  *url.URL
	annotations map[string]string
	ingressTLS  []extensions.IngressTLS
}

func createSparkUIIngress(app *v1beta2.SparkApplication, service SparkService, ingressURL *url.URL, kubeClient clientset.Interface) (*SparkIngress, error) {
	ingressResourceAnnotations := getIngressResourceAnnotations(app)
	ingressTlsHosts := getIngressTlsHosts(app)

	ingressURLPath := ingressURL.Path
	// If we're serving on a subpath, we need to ensure we create capture groups
	if ingressURLPath != "" && ingressURLPath != "/" {
		ingressURLPath = ingressURLPath + "(/|$)(.*)"
	}

	ingress := extensions.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:            getDefaultUIIngressName(app),
			Namespace:       app.Namespace,
			Labels:          getResourceLabels(app),
			OwnerReferences: []metav1.OwnerReference{*getOwnerReference(app)},
		},
		Spec: extensions.IngressSpec{
			Rules: []extensions.IngressRule{{
				Host: ingressURL.Host,
				IngressRuleValue: extensions.IngressRuleValue{
					HTTP: &extensions.HTTPIngressRuleValue{
						Paths: []extensions.HTTPIngressPath{{
							Backend: extensions.IngressBackend{
								ServiceName: service.serviceName,
								ServicePort: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: service.servicePort,
								},
							},
							Path: ingressURLPath,
						}},
					},
				},
			}},
		},
	}

	if len(ingressResourceAnnotations) != 0 {
		ingress.ObjectMeta.Annotations = ingressResourceAnnotations
	}

	// If we're serving on a subpath, we need to ensure we use the capture groups
	if ingressURL.Path != "" && ingressURL.Path != "/" {
		if ingress.ObjectMeta.Annotations == nil {
			ingress.ObjectMeta.Annotations = make(map[string]string)
		}
		ingress.ObjectMeta.Annotations["nginx.ingress.kubernetes.io/rewrite-target"] = "/$2"
	}
	if len(ingressTlsHosts) != 0 {
		ingress.Spec.TLS = ingressTlsHosts
	}
	glog.Infof("Creating an Ingress %s for the Spark UI for application %s", ingress.Name, app.Name)
	_, err := kubeClient.ExtensionsV1beta1().Ingresses(ingress.Namespace).Create(context.TODO(), &ingress, metav1.CreateOptions{})

	if err != nil {
		return nil, err
	}
	return &SparkIngress{
		ingressName: ingress.Name,
		ingressURL:  ingressURL,
		annotations: ingress.Annotations,
		ingressTLS:  ingress.Spec.TLS,
	}, nil
}

func createSparkUIService(
	app *v1beta2.SparkApplication,
	kubeClient clientset.Interface) (*SparkService, error) {
	portName := getUIServicePortName(app)
	port, err := getUIServicePort(app)
	if err != nil {
		return nil, fmt.Errorf("invalid Spark UI servicePort: %d", port)
	}
	tPort, err := getUITargetPort(app)
	if err != nil {
		return nil, fmt.Errorf("invalid Spark UI targetPort: %d", tPort)
	}
	service := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            getDefaultUIServiceName(app),
			Namespace:       app.Namespace,
			Labels:          getResourceLabels(app),
			OwnerReferences: []metav1.OwnerReference{*getOwnerReference(app)},
		},
		Spec: apiv1.ServiceSpec{
			Ports: []apiv1.ServicePort{
				{
					Name: portName,
					Port: port,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: tPort,
					},
				},
			},
			Selector: map[string]string{
				config.SparkAppNameLabel: app.Name,
				config.SparkRoleLabel:    config.SparkDriverRole,
			},
			Type: getUIServiceType(app),
		},
	}

	serviceAnnotations := getServiceAnnotations(app)
	if len(serviceAnnotations) != 0 {
		service.ObjectMeta.Annotations = serviceAnnotations
	}

	glog.Infof("Creating a service %s for the Spark UI for application %s", service.Name, app.Name)
	service, err = kubeClient.CoreV1().Services(app.Namespace).Create(context.TODO(), service, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return &SparkService{
		serviceName:        service.Name,
		serviceType:        service.Spec.Type,
		servicePort:        service.Spec.Ports[0].Port,
		servicePortName:    service.Spec.Ports[0].Name,
		targetPort:         service.Spec.Ports[0].TargetPort,
		serviceIP:          service.Spec.ClusterIP,
		serviceAnnotations: serviceAnnotations,
	}, nil
}

// getWebUITargetPort attempts to get the Spark web UI port from configuration property spark.ui.port
// in Spec.SparkConf if it is present, otherwise the default port is returned.
// Note that we don't attempt to get the port from Spec.SparkConfigMap.
func getUITargetPort(app *v1beta2.SparkApplication) (int32, error) {
	portStr, ok := app.Spec.SparkConf[sparkUIPortConfigurationKey]
	if ok {
		port, err := strconv.Atoi(portStr)
		return int32(port), err
	}
	return defaultSparkWebUIPort, nil
}

func getUIServicePort(app *v1beta2.SparkApplication) (int32, error) {
	if app.Spec.SparkUIOptions == nil {
		return getUITargetPort(app)
	}
	port := app.Spec.SparkUIOptions.ServicePort
	if port != nil {
		return *port, nil
	}
	return defaultSparkWebUIPort, nil
}

func getUIServicePortName(app *v1beta2.SparkApplication) string {
	if app.Spec.SparkUIOptions == nil {
		return defaultSparkWebUIPortName
	}
	portName := app.Spec.SparkUIOptions.ServicePortName
	if portName != nil {
		return *portName
	}
	return defaultSparkWebUIPortName
}
