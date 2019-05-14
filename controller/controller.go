package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"k8s.io/kubernetes/pkg/api"
	uapi "k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/client/restclient"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/watch"

	"github.com/jenkins-x/exposecontroller/exposestrategy"

	oclient "github.com/openshift/origin/pkg/client"
	oauthapi "github.com/openshift/origin/pkg/oauth/api"
	oauthapiv1 "github.com/openshift/origin/pkg/oauth/api/v1"
	"gopkg.in/v2/yaml"
)

const (
	ExposeConfigURLProtocol                       = "expose.config.fabric8.io/url-protocol"
	ExposeConfigURLKeyAnnotation                  = "expose.config.fabric8.io/url-key"
	ExposeConfigHostKeyAnnotation                 = "expose.config.fabric8.io/host-key"
	ExposeConfigClusterPathKeyAnnotation          = "expose.config.fabric8.io/path-key"
	ExposeConfigClusterIPKeyAnnotation            = "expose.config.fabric8.io/clusterip-key"
	ExposeConfigClusterIPPortKeyAnnotation        = "expose.config.fabric8.io/clusterip-port-key"
	ExposeConfigClusterIPPortIfEmptyKeyAnnotation = "expose.config.fabric8.io/clusterip-port-if-empty-key"
	ExposeConfigApiServerKeyAnnotation            = "expose.config.fabric8.io/apiserver-key"
	ExposeConfigApiServerURLKeyAnnotation         = "expose.config.fabric8.io/apiserver-url-key"
	ExposeConfigConsoleURLKeyAnnotation           = "expose.config.fabric8.io/console-url-key"
	ExposeConfigApiServerProtocolKeyAnnotation    = "expose.config.fabric8.io/apiserver-protocol-key"
	ExposeConfigOAuthAuthorizeURLKeyAnnotation    = "expose.config.fabric8.io/oauth-authorize-url-key"

	ExposeConfigYamlAnnotation = "expose.config.fabric8.io/config-yaml"

	OAuthAuthorizeUrlEnvVar = "OAUTH_AUTHORIZE_URL"

	updateOnChangeAnnotation = "configmap.fabric8.io/update-on-change"
)

type Controller struct {
	client *client.Client

	svcController *framework.Controller
	svcLister     cache.StoreToServiceLister

	config *Config

	recorder record.EventRecorder

	stopCh chan struct{}
}

func NewController(
	kubeClient *client.Client,
	restClientConfig *restclient.Config,
	encoder runtime.Encoder,
	resyncPeriod time.Duration, namespace string, config *Config) (*Controller, error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(kubeClient.Events(namespace))

	glog.Infof("NewController %v", config.HTTP)

	c := Controller{
		client: kubeClient,
		stopCh: make(chan struct{}),
		config: config,
		recorder: eventBroadcaster.NewRecorder(api.EventSource{
			Component: "expose-controller",
		}),
	}

	strategy, err := exposestrategy.New(config.Exposer, config.Domain, config.UrlTemplate, config.NodeIP, config.RouteHost, config.PathMode, config.RouteUsePath, config.HTTP, config.TLSAcme, config.TLSSecretName, config.TLSUseWildcard, config.IngressClass, kubeClient, restClientConfig, encoder)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new strategy")
	}

	var oc *oclient.Client = nil
	authorizeURL := ""
	if isOpenShift(kubeClient) {
		// register openshift schemas
		oauthapi.AddToScheme(api.Scheme)
		oauthapiv1.AddToScheme(api.Scheme)

		ocfg := *restClientConfig
		ocfg.APIPath = ""
		ocfg.GroupVersion = nil
		ocfg.NegotiatedSerializer = nil
		oc, _ = oclient.New(&ocfg)

		authorizeURL = findOAuthAuthorizeURL()
		if len(authorizeURL) == 0 {
			authorizeURL = os.Getenv(OAuthAuthorizeUrlEnvVar)
		}
		if len(authorizeURL) == 0 {
			authorizeURL = config.ApiServer
			if len(authorizeURL) == 0 {
				authorizeURL = findApiServerFromNode(kubeClient)
				config.ApiServer = authorizeURL
			}
			if len(authorizeURL) > 0 {
				if !strings.HasPrefix(authorizeURL, "http:") && !strings.HasPrefix(authorizeURL, "https:") {
					authorizeURL = "https://" + authorizeURL
				}
				authPath := config.AuthorizePath
				if len(authPath) == 0 {
					authPath = "/oauth/authorize"
				}
				if !strings.HasPrefix(authPath, "/") {
					authPath = "/" + authPath
				}
				authorizeURL = strings.TrimSuffix(authorizeURL, "/") + authPath
			}
		}
		glog.Infof("Using OAuth Authorize URL: %s", authorizeURL)
		if len(authorizeURL) == 0 {
			glog.Warningf("Please use $%s to define the OAuth Authorize URL!", OAuthAuthorizeUrlEnvVar)
		}
	}
	if len(config.ApiServerProtocol) == 0 {
		config.ApiServerProtocol = kubernetesServiceProtocol(kubeClient)
	}

	c.svcLister.Store, c.svcController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc:  serviceListFunc(c.client, namespace),
			WatchFunc: serviceWatchFunc(c.client, namespace),
		},
		&api.Service{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				svc := obj.(*api.Service)
				if svc.Labels[exposestrategy.ExposeLabel.Key] == exposestrategy.ExposeLabel.Value ||
					svc.Annotations[exposestrategy.ExposeAnnotation.Key] == exposestrategy.ExposeAnnotation.Value ||
					svc.Annotations[exposestrategy.InjectAnnotation.Key] == exposestrategy.InjectAnnotation.Value {
					if !isServiceWhitelisted(svc.GetName(), config) {
						return
					}
					err := strategy.Add(svc)
					if err != nil {
						glog.Errorf("Add failed: %v", err)
					}
					updateRelatedResources(kubeClient, oc, svc, config, authorizeURL)
				}
			},
			UpdateFunc: func(oldObj interface{}, newObj interface{}) {
				svc := newObj.(*api.Service)
				if svc.Labels[exposestrategy.ExposeLabel.Key] == exposestrategy.ExposeLabel.Value ||
					svc.Annotations[exposestrategy.ExposeAnnotation.Key] == exposestrategy.ExposeAnnotation.Value ||
					svc.Annotations[exposestrategy.InjectAnnotation.Key] == exposestrategy.InjectAnnotation.Value {
					if !isServiceWhitelisted(svc.GetName(), config) {
						return
					}
					err := strategy.Add(svc)
					if err != nil {
						glog.Errorf("Add failed: %v", err)
					}
					updateRelatedResources(kubeClient, oc, svc, config, authorizeURL)
				} else {
					oldSvc := oldObj.(*api.Service)
					if oldSvc.Labels[exposestrategy.ExposeLabel.Key] == exposestrategy.ExposeLabel.Value ||
						oldSvc.Annotations[exposestrategy.ExposeAnnotation.Key] == exposestrategy.ExposeAnnotation.Value ||
						svc.Annotations[exposestrategy.InjectAnnotation.Key] == exposestrategy.InjectAnnotation.Value {
						if !isServiceWhitelisted(svc.GetName(), config) {
							return
						}
						err := strategy.Remove(svc)
						if err != nil {
							glog.Errorf("Remove failed: %v", err)
						}
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				svc, ok := obj.(cache.DeletedFinalStateUnknown)
				if ok {
					// service key is in the form namespace/name
					split := strings.Split(svc.Key, "/")
					ns := split[0]
					name := split[1]
					if !isServiceWhitelisted(name, config) {
						return
					}
					err := strategy.Remove(&api.Service{ObjectMeta: api.ObjectMeta{Namespace: ns, Name: name}})
					if err != nil {
						glog.Errorf("Remove failed: %v", err)
					}
				}
			},
		},
	)

	return &c, nil
}

// isServiceWhitelisted checks if a service is white-listed in the controller configuration, allow all services if
// the white-list is empty
func isServiceWhitelisted(service string, config *Config) bool {
	services := config.Services
	if len(services) == 0 {
		return true
	}
	for _, s := range services {
		if s == service {
			return true
		}
	}
	return false
}

// findApiServerFromNode lets try default the API server URL by detecting minishift/minikube for single node clusters
func findApiServerFromNode(c *client.Client) string {
	nodes, err := c.Nodes().List(api.ListOptions{})
	if err != nil {
		glog.Errorf("Failed to list nodes to detect minishift: %v", err)
		return ""
	}
	items := nodes.Items
	if len(items) != 1 {
		glog.Errorf("Number of nodes is %d. We need 1 to detect minishift. Please use  to list nodes to detect minishift: %v", len(items), err)
		return ""
	}
	node := items[0]
	port := "8443"
	ann := node.Annotations
	host := ""
	if ann != nil {
		host = ann["kubernetes.io/hostname"]
	}
	if len(host) == 0 {
		host = node.Spec.ExternalID
	}
	if len(host) == 0 {
		host = node.Name
	}
	if len(host) > 0 {
		return host + ":" + port
	}
	return ""

}

func isOpenShift(c *client.Client) bool {
	res, err := c.Get().AbsPath("").DoRaw()
	if err != nil {
		glog.Errorf("Could not discover the type of your installation: %v", err)
		return false
	}

	var rp uapi.RootPaths
	err = json.Unmarshal(res, &rp)
	if err != nil {
		glog.Errorf("Could not discover the type of your installation: %v", err)
		return false
	}
	for _, p := range rp.Paths {
		if p == "/oapi" {
			return true
		}
	}
	return false
}

func updateRelatedResources(c *client.Client, oc *oclient.Client, svc *api.Service, config *Config, authorizeURL string) {
	updateServiceConfigMap(c, oc, svc, config, authorizeURL)
	if oc != nil {
		updateServiceOAuthClient(oc, svc)
	}

	exposeURL := svc.Annotations[exposestrategy.ExposeAnnotationKey]
	if len(exposeURL) > 0 {
		updateOtherConfigMaps(c, oc, svc, config, exposeURL)
	}
}

func kubernetesServiceProtocol(c *client.Client) string {
	hasHttp := false
	svc, err := c.Services("default").Get("kubernetes")
	if err != nil {
		glog.Warningf("Could not find kubernetes service in the default namespace so we could not detect whether to use http or https as the apiserver protocol. Error: %v", err)
	} else {
		for _, port := range svc.Spec.Ports {
			if port.Name == "https" || port.Port == 443 {
				return "https"
			}
			if port.Name == "http" || port.Port == 80 {
				hasHttp = true
			}
		}
	}
	if hasHttp {
		return "http"
	}
	return "https"
}

func GetServicePort(svc *api.Service) string {
	for _, port := range svc.Spec.Ports {
		tp := port.TargetPort.StrVal
		if tp != "" {
			return tp
		}
		i := port.TargetPort.IntVal
		if i > 0 {
			return strconv.Itoa(int(i))
		}
	}
	return ""
}

type ConfigYaml struct {
	Key        string
	Expression string
	Prefix     string
	Suffix     string
}

func updateServiceConfigMap(c *client.Client, oc *oclient.Client, svc *api.Service, config *Config, authorizeURL string) {
	name := svc.Name
	ns := svc.Namespace
	cm, err := c.ConfigMaps(ns).Get(name)
	apiserverURL := ""
	apiserver := ""
	apiserverProtocol := ""
	if err == nil {
		updated := false
		apiserver = config.ApiServer
		apiserverProtocol = config.ApiServerProtocol
		consoleURL := config.ConsoleURL

		if len(apiserver) > 0 {
			apiserverURL = apiserverProtocol + "://" + apiserver
			apiServerKey := cm.Annotations[ExposeConfigApiServerKeyAnnotation]
			if len(apiServerKey) > 0 {
				if cm.Data[apiServerKey] != apiserver {
					cm.Data[apiServerKey] = apiserver
					updated = true
				}
			}
			apiServerURLKey := cm.Annotations[ExposeConfigApiServerURLKeyAnnotation]
			if len(apiServerURLKey) > 0 {
				if cm.Data[apiServerURLKey] != apiserverURL {
					cm.Data[apiServerURLKey] = apiserverURL
					updated = true
				}
			}
			if len(consoleURL) == 0 {
				if isOpenShift(c) {
					consoleURL = urlJoin(apiserverURL, "/console")
				}
			}
		}
		if len(consoleURL) > 0 {
			consoleURLKey := cm.Annotations[ExposeConfigConsoleURLKeyAnnotation]
			if len(consoleURLKey) > 0 {
				if cm.Data[consoleURLKey] != consoleURL {
					cm.Data[consoleURLKey] = consoleURL
					updated = true
				}
			}
		}
		apiserverProtocolKey := cm.Annotations[ExposeConfigApiServerProtocolKeyAnnotation]
		if len(apiserverProtocolKey) > 0 {
			if cm.Data[apiserverProtocolKey] != apiserverProtocol {
				cm.Data[apiserverProtocolKey] = apiserverProtocol
				updated = true
			}
		}
		if len(authorizeURL) > 0 && oc != nil {
			authorizeURLKey := cm.Annotations[ExposeConfigOAuthAuthorizeURLKeyAnnotation]
			if len(authorizeURLKey) > 0 {
				if cm.Data[authorizeURLKey] != authorizeURL {
					cm.Data[authorizeURLKey] = authorizeURL
					updated = true
				}
			}
		}

		clusterIP := svc.Spec.ClusterIP
		if clusterIP != "" {
			clusterIPKey := firstMapValue(ExposeConfigClusterIPKeyAnnotation, svc.Annotations, cm.Annotations)
			clusterIPPortKey := firstMapValue(ExposeConfigClusterIPPortKeyAnnotation, svc.Annotations, cm.Annotations)
			clusterIPPortIfEmptyKey := firstMapValue(ExposeConfigClusterIPPortIfEmptyKeyAnnotation, svc.Annotations, cm.Annotations)

			if clusterIPKey != "" {
				if cm.Data[clusterIPKey] != clusterIP {
					cm.Data[clusterIPKey] = clusterIP
					updated = true
				}
			}

			port := GetServicePort(svc)
			if port != "" {
				clusterIPAndPort := clusterIP + ":" + port

				if clusterIPPortKey != "" {
					if cm.Data[clusterIPPortKey] != clusterIPAndPort {
						cm.Data[clusterIPPortKey] = clusterIPAndPort
						updated = true
					}
				}
				if clusterIPPortIfEmptyKey != "" {
					if cm.Data[clusterIPPortIfEmptyKey] == "" {
						cm.Data[clusterIPPortIfEmptyKey] = clusterIPAndPort
						updated = true
					}
				}
			}
		}
		exposeURL := svc.Annotations[exposestrategy.ExposeAnnotationKey]
		if len(exposeURL) > 0 {
			host := ""
			url, err := url.Parse(exposeURL)
			if err != nil {
				glog.Errorf("Failed to parse expose URL %s for service %s  error: %v", exposeURL, name, err)

			} else {
				host = url.Host
			}
			urlKey := cm.Annotations[ExposeConfigURLKeyAnnotation]
			domainKey := cm.Annotations[ExposeConfigHostKeyAnnotation]
			if len(urlKey) > 0 {
				if cm.Data[urlKey] != exposeURL {
					cm.Data[urlKey] = exposeURL
					updated = true
				}
			}
			if len(host) > 0 && len(domainKey) > 0 {
				if cm.Data[domainKey] != host {
					cm.Data[domainKey] = host
					updated = true
				}
			}

			pathKey := cm.Annotations[ExposeConfigClusterPathKeyAnnotation]
			if pathKey != "" {
				path := urlPath(exposeURL)
				if cm.Data[pathKey] != path {
					cm.Data[pathKey] = path
					updated = true
				}
				glog.Infof("Found key %s and has path %s\n", pathKey, path)
			}

			configYaml := svc.Annotations[ExposeConfigYamlAnnotation]
			if configYaml != "" {
				fmt.Printf("Procssing ConfigYaml on service %s\n", svc.Name)
				configs := []ConfigYaml{}
				err := yaml.Unmarshal([]byte(configYaml), &configs)
				if err != nil {
					glog.Errorf("Failed to unmarshal Config YAML on service %s due to %s : YAML: %s", svc.Name, err, configYaml)
				} else {
					values := map[string]string{
						"host":              host,
						"url":               exposeURL,
						"apiserver":         apiserver,
						"apiserverURL":      apiserverURL,
						"apiserverProtocol": apiserverProtocol,
						"consoleURL":        consoleURL,
					}
					fmt.Printf("Loading ConfigYaml configurations %#v\n", configs)
					for _, c := range configs {
						if c.UpdateConfigMap(cm, values) {
							updated = true
						}
					}
				}
			}
		}
		if updated {
			glog.Infof("Updating ConfigMap %s/%s", ns, name)
			_, err = c.ConfigMaps(ns).Update(cm)
			if err != nil {
				glog.Errorf("Failed to update ConfigMap %s error: %v", name, err)
			}
			err = rollingUpgradeDeployments(cm, c)
			if err != nil {
				glog.Errorf("Failed to update Deployments after change to ConfigMap %s error: %v", name, err)
			}
		}
	}
}

// returns the path starting with a `/` character for the given URL
func urlPath(urlText string) string {
	answer := "/"
	u, err := url.Parse(urlText)
	if err != nil {
		glog.Warningf("Could not parse exposeUrl: %s due to: %s", urlText, err)
	} else {
		if u.Path != "" {
			answer = u.Path
		}
		if !strings.HasPrefix(answer, "/") {
			answer = "/" + answer
		}
	}
	return answer
}

// firstMapValue returns the first value in the map which is not empty
func firstMapValue(key string, maps ...map[string]string) string {
	for _, m := range maps {
		if m != nil {
			v := m[key]
			if v != "" {
				return v
			}
		}
	}
	return ""
}

func (c *ConfigYaml) UpdateConfigMap(configMap *api.ConfigMap, values map[string]string) bool {
	key := c.Key
	if key == "" {
		glog.Warningf("ConfigMap %s does not have a key in ConfigYaml settings %#v\n", configMap.Name, c)
		return false
	}
	expValue := values[c.Expression]
	if expValue == "" {
		glog.Warningf("Could not calculate expression %s from the ConfigYaml settings %#v possible values are %v\n", c.Expression, c, values)
		return false
	}
	value := configMap.Data[key]
	if value == "" {
		glog.Warningf("ConfigMap %s does not have a key %s when trying to apply the ConfigYaml settings %#v\n", configMap.Name, key, c)
		return false
	}
	lines := strings.Split(value, "\n")
	var buffer bytes.Buffer
	for _, line := range lines {
		if strings.HasPrefix(line, c.Prefix) {
			buffer.WriteString(c.Prefix + expValue + c.Suffix)
		} else {
			buffer.WriteString(line)
		}
		buffer.WriteString("\n")
	}
	newValue := buffer.String()
	if newValue != value {
		configMap.Data[key] = newValue
		return true
	}
	return false
}

func urlJoin(s1 string, s2 string) string {
	return strings.TrimSuffix(s1, "/") + "/" + strings.TrimPrefix(s2, "/")
}

// updateOtherConfigMaps lets update all other configmaps which want to be injected by this svc exposeURL
func updateOtherConfigMaps(c *client.Client, oc *oclient.Client, svc *api.Service, config *Config, exposeURL string) error {
	serviceName := svc.Name
	annotationKey := "expose.service-key.config.fabric8.io/" + serviceName
	annotationFullKey := "expose-full.service-key.config.fabric8.io/" + serviceName
	annotationNoProtocolKey := "expose-no-protocol.service-key.config.fabric8.io/" + serviceName
	annotationNoPathKey := "expose-no-path.service-key.config.fabric8.io/" + serviceName
	annotationFullNoProtocolKey := "expose-full-no-protocol.service-key.config.fabric8.io/" + serviceName
	ns := svc.Namespace
	cms, err := c.ConfigMaps(ns).List(api.ListOptions{})
	if err != nil {
		return err
	}
	for _, cm := range cms.Items {
		update := false
		updateKey := cm.Annotations[annotationKey]
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if len(updateKey) > 0 {
			exposeURL = strings.TrimSuffix(exposeURL, "/")
			keys := strings.Split(updateKey, ",")
			for _, key := range keys {
				value := cm.Data[key]
				if value != exposeURL {
					cm.Data[key] = exposeURL
					glog.Infof("Updating ConfigMap %s in namespace %s with key %s", cm.Name, ns, key)
					update = true
				}
			}
		}
		updateKey = cm.Annotations[annotationFullKey]
		if len(updateKey) > 0 {
			if !strings.HasSuffix(exposeURL, "/") {
				exposeURL += "/"
			}
			keys := strings.Split(updateKey, ",")
			for _, key := range keys {
				value := cm.Data[key]
				if value != exposeURL {
					cm.Data[key] = exposeURL
					glog.Infof("Updating ConfigMap %s in namespace %s with key %s", cm.Name, ns, key)
					update = true
				}
			}
		}
		updateKey = cm.Annotations[annotationNoPathKey]
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if len(updateKey) > 0 {
			u, err := url.Parse(exposeURL)
			if err != nil {
				glog.Warningf("Failed to parse URL %s due to %s", exposeURL, err)
			} else {
				u.Path = "/"
				noPathURL := u.String()
				keys := strings.Split(updateKey, ",")
				for _, key := range keys {
					value := cm.Data[key]
					if value != noPathURL {
						cm.Data[key] = noPathURL
						glog.Infof("Updating ConfigMap %s in namespace %s with key %s", cm.Name, ns, key)
						update = true
					}
				}
			}
		}
		updateKey = cm.Annotations[annotationNoProtocolKey]
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if len(updateKey) > 0 {
			exposeURL = strings.TrimSuffix(exposeURL, "/")
			exposeURL = strings.TrimPrefix(exposeURL, "http://")
			exposeURL = strings.TrimPrefix(exposeURL, "https://")
			keys := strings.Split(updateKey, ",")
			for _, key := range keys {
				value := cm.Data[key]
				if value != exposeURL {
					cm.Data[key] = exposeURL
					glog.Infof("Updating ConfigMap %s in namespace %s with key %s", cm.Name, ns, key)
					update = true
				}
			}
		}
		updateKey = cm.Annotations[annotationFullNoProtocolKey]
		if len(updateKey) > 0 {
			if !strings.HasSuffix(exposeURL, "/") {
				exposeURL += "/"
			}
			exposeURL = strings.TrimPrefix(exposeURL, "http://")
			exposeURL = strings.TrimPrefix(exposeURL, "https://")
			keys := strings.Split(updateKey, ",")
			for _, key := range keys {
				value := cm.Data[key]
				if value != exposeURL {
					cm.Data[key] = exposeURL
					glog.Infof("Updating ConfigMap %s in namespace %s with key %s", cm.Name, ns, key)
					update = true
				}
			}
		}
		updateKey = cm.Annotations[ExposeConfigURLProtocol]
		if len(updateKey) > 0 {
			protocol := "https"
			if config.HTTP {
				protocol = "http"
			}
			keys := strings.Split(updateKey, ",")
			for _, key := range keys {
				value := cm.Data[key]
				if value != protocol {
					cm.Data[key] = protocol
					glog.Infof("Updating ConfigMap %s in namespace %s with key %s", cm.Name, ns, key)
					update = true
				}
			}
		}
		if update {
			_, err = c.ConfigMaps(ns).Update(&cm)
			if err != nil {
				return fmt.Errorf("Failed to update ConfigMap %s in namespace %s with key %s due to %v", cm.Name, ns, updateKey, err)
			}
		}
	}
	return nil
}

// findOAuthAuthorizeURL uses this endpoint: https://github.com/openshift/origin/pull/10845
func findOAuthAuthorizeURL() string {
	url := "https://openshift.default.svc/.well-known/oauth-authorization-server"
	// test data
	//url := "https://gist.githubusercontent.com/jstrachan/dbb2066d89810ef1fa53c1df118ccb41/raw/e60a2d42e11930eef13a4264d35514ffd365c8af/dummy.json"
	r, err := http.Get(url)
	if err != nil {
		glog.Warningf("Failed to load url %s got: %v", url, err)
		return ""
	}
	defer r.Body.Close()

	var target OAuthServer
	err = json.NewDecoder(r.Body).Decode(&target)
	if err != nil {
		glog.Warningf("Failed to decode JSON from %s got: %v", url, err)
		return ""
	}
	return target.AuthorizationEndpoint
}

type OAuthServer struct {
	Issuer                string `json:"issuer,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
}

func updateServiceOAuthClient(oc *oclient.Client, svc *api.Service) {
	name := svc.Name
	exposeURL := svc.Annotations[exposestrategy.ExposeAnnotationKey]
	if len(exposeURL) > 0 {
		oauthClient, err := oc.OAuthClients().Get(name)
		if err == nil {
			redirects := oauthClient.RedirectURIs
			found := false
			for _, uri := range redirects {
				if uri == exposeURL {
					found = true
					break
				}
			}
			if !found {
				oauthClient.RedirectURIs = append(redirects, exposeURL)
				glog.Infof("Deleting OAuthClient %s", name)
				err = oc.OAuthClients().Delete(name)
				if err != nil {
					glog.Errorf("Failed to delete OAuthClient %s error: %v", name, err)
					return
				}
				oauthClient.ResourceVersion = ""
				glog.Infof("Creating OAuthClient %s with redirectURIs %v", name, oauthClient.RedirectURIs)
				_, err = oc.OAuthClients().Create(oauthClient)
				if err != nil {
					glog.Errorf("Failed to delete OAuthClient %s error: %v", name, err)
					return
				}
			}
		}
	}
}

// Run starts the controller.
func (c *Controller) Run() {
	glog.Infof("starting expose controller")

	go c.svcController.Run(c.stopCh)

	<-c.stopCh
}

func (c *Controller) Stop() {
	glog.Infof("stopping expose controller")

	close(c.stopCh)
}

func (c *Controller) Hasrun() bool {
	return c.svcController.HasSynced()
}

func serviceListFunc(c *client.Client, ns string) func(api.ListOptions) (runtime.Object, error) {
	return func(opts api.ListOptions) (runtime.Object, error) {
		return c.Services(ns).List(opts)
	}
}

func serviceWatchFunc(c *client.Client, ns string) func(options api.ListOptions) (watch.Interface, error) {
	return func(options api.ListOptions) (watch.Interface, error) {
		return c.Services(ns).Watch(options)
	}
}
