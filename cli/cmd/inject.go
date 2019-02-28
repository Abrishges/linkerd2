package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	k8sMeta "k8s.io/apimachinery/pkg/api/meta"
	k8sResource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

const (
	// LocalhostDNSNameOverride allows override of the controlPlaneDNS. This
	// must be in absolute form for the proxy to special-case it.
	LocalhostDNSNameOverride = "localhost."
	// ControlPlanePodName default control plane pod name.
	ControlPlanePodName    = "linkerd-controller"
	PodNamespaceEnvVarName = "K8S_NS"

	// for inject reports

	hostNetworkDesc    = "pods do not use host networking"
	sidecarDesc        = "pods do not have a 3rd party proxy or initContainer already injected"
	injectDisabledDesc = "pods are not annotated to disable injection"
	unsupportedDesc    = "at least one resource injected"
	udpDesc            = "pod specs do not include UDP ports"
)

type injectOptions struct {
	*proxyConfigOptions
}

type identityConfig struct {
	trustDomain, trustAnchorsPEM string
}

type resourceTransformerInject struct{}

// InjectYAML processes resource definitions and outputs them after injection in out
func InjectYAML(in io.Reader, out io.Writer, report io.Writer, options *injectOptions) error {
	return ProcessYAML(in, out, report, options, resourceTransformerInject{})
}

func runInjectCmd(inputs []io.Reader, errWriter, outWriter io.Writer, options *injectOptions) int {
	return transformInput(inputs, errWriter, outWriter, options, resourceTransformerInject{})
}

// objMeta provides a generic struct to parse the names of Kubernetes objects
type objMeta struct {
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
}

func newInjectOptions() *injectOptions {
	return &injectOptions{
		proxyConfigOptions: newProxyConfigOptions(),
	}
}

func newCmdInject() *cobra.Command {
	options := newInjectOptions()

	cmd := &cobra.Command{
		Use:   "inject [flags] CONFIG-FILE",
		Short: "Add the Linkerd proxy to a Kubernetes config",
		Long: `Add the Linkerd proxy to a Kubernetes config.

You can inject resources contained in a single file, inside a folder and its
sub-folders, or coming from stdin.`,
		Example: `  # Inject all the deployments in the default namespace.
  kubectl get deploy -o yaml | linkerd inject - | kubectl apply -f -

  # Download a resource and inject it through stdin.
  curl http://url.to/yml | linkerd inject - | kubectl apply -f -

  # Inject all the resources inside a folder and its sub-folders.
  linkerd inject <folder> | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {

			if len(args) < 1 {
				return fmt.Errorf("please specify a kubernetes resource file")
			}

			if err := options.validate(); err != nil {
				return err
			}

			in, err := read(args[0])
			if err != nil {
				return err
			}

			exitCode := uninjectAndInject(in, stderr, stdout, options)
			os.Exit(exitCode)
			return nil
		},
	}

	addProxyConfigFlags(cmd, options.proxyConfigOptions)

	return cmd
}

func uninjectAndInject(inputs []io.Reader, errWriter, outWriter io.Writer, options *injectOptions) int {
	var out bytes.Buffer
	if exitCode := runUninjectSilentCmd(inputs, errWriter, &out, nil); exitCode != 0 {
		return exitCode
	}
	return runInjectCmd([]io.Reader{&out}, errWriter, outWriter, options)
}

// injectObjectMeta adds linkerd labels & annotations to the provided ObjectMeta.
func injectObjectMeta(t *metav1.ObjectMeta, k8sLabels map[string]string, options *injectOptions) {
	if t.Annotations == nil {
		t.Annotations = make(map[string]string)
	}
	t.Annotations[k8s.CreatedByAnnotation] = k8s.CreatedByAnnotationValue()
	t.Annotations[k8s.ProxyVersionAnnotation] = options.linkerdVersion

	if t.Labels == nil {
		t.Labels = make(map[string]string)
	}
	t.Labels[k8s.ControllerNSLabel] = controlPlaneNamespace
	for k, v := range k8sLabels {
		t.Labels[k] = v
	}

	if t.Annotations[k8s.IdentityModeAnnotation] == k8s.IdentityModeOptional {
		t.Annotations[k8s.IdentityModeAnnotation] = k8s.IdentityModeDefault
	}

	if t.Annotations[k8s.IdentityModeAnnotation] == "" || options.enableTLS() {
		t.Annotations[k8s.IdentityModeAnnotation] = k8s.IdentityModeDefault
	} else {
		t.Annotations[k8s.IdentityModeAnnotation] = k8s.IdentityModeDisabled
	}
}

// injectPodSpec adds linkerd sidecars to the provided PodSpec.
func injectPodSpec(t *corev1.PodSpec, identity k8s.TLSIdentity, controlPlaneDNSNameOverride string, options *injectOptions) {

	f := false
	inboundSkipPorts := append(options.ignoreInboundPorts, options.proxyControlPort, options.proxyMetricsPort)
	inboundSkipPortsStr := make([]string, len(inboundSkipPorts))
	for i, p := range inboundSkipPorts {
		inboundSkipPortsStr[i] = strconv.Itoa(int(p))
	}

	outboundSkipPortsStr := make([]string, len(options.ignoreOutboundPorts))
	for i, p := range options.ignoreOutboundPorts {
		outboundSkipPortsStr[i] = strconv.Itoa(int(p))
	}

	initArgs := []string{
		"--incoming-proxy-port", fmt.Sprintf("%d", options.inboundPort),
		"--outgoing-proxy-port", fmt.Sprintf("%d", options.outboundPort),
		"--proxy-uid", fmt.Sprintf("%d", options.proxyUID),
	}

	if len(inboundSkipPortsStr) > 0 {
		initArgs = append(initArgs, "--inbound-ports-to-ignore")
		initArgs = append(initArgs, strings.Join(inboundSkipPortsStr, ","))
	}

	if len(outboundSkipPortsStr) > 0 {
		initArgs = append(initArgs, "--outbound-ports-to-ignore")
		initArgs = append(initArgs, strings.Join(outboundSkipPortsStr, ","))
	}

	controlPlaneDNS := fmt.Sprintf("linkerd-destination.%s.svc.cluster.local", controlPlaneNamespace)
	if controlPlaneDNSNameOverride != "" {
		controlPlaneDNS = controlPlaneDNSNameOverride
	}

	metricsPort := intstr.IntOrString{
		IntVal: int32(options.proxyMetricsPort),
	}

	proxyProbe := corev1.Probe{
		Handler: corev1.Handler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/metrics",
				Port: metricsPort,
			},
		},
		InitialDelaySeconds: 10,
	}

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}

	if options.proxyCPURequest != "" {
		resources.Requests["cpu"] = k8sResource.MustParse(options.proxyCPURequest)
	}

	if options.proxyMemoryRequest != "" {
		resources.Requests["memory"] = k8sResource.MustParse(options.proxyMemoryRequest)
	}

	if options.proxyCPULimit != "" {
		resources.Limits["cpu"] = k8sResource.MustParse(options.proxyCPULimit)
	}

	if options.proxyMemoryLimit != "" {
		resources.Limits["memory"] = k8sResource.MustParse(options.proxyMemoryLimit)
	}

	profileSuffixes := "."
	if options.disableExternalProfiles {
		profileSuffixes = "svc.cluster.local."
	}
	identity.Namespace = fmt.Sprintf("$(%s)", PodNamespaceEnvVarName)
	sidecar := corev1.Container{
		Name:                     k8s.ProxyContainerName,
		Image:                    options.taggedProxyImage(),
		ImagePullPolicy:          corev1.PullPolicy(options.imagePullPolicy),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: &options.proxyUID,
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          "linkerd-proxy",
				ContainerPort: int32(options.inboundPort),
			},
			{
				Name:          "linkerd-metrics",
				ContainerPort: int32(options.proxyMetricsPort),
			},
		},
		Resources: resources,
		Env: []corev1.EnvVar{
			{Name: "LINKERD2_PROXY_LOG", Value: options.proxyLogLevel},
			{
				Name:  "LINKERD2_PROXY_CONTROL_URL",
				Value: fmt.Sprintf("tcp://%s:%d", controlPlaneDNS, options.destinationAPIPort),
			},
			{Name: "LINKERD2_PROXY_CONTROL_LISTENER", Value: fmt.Sprintf("tcp://0.0.0.0:%d", options.proxyControlPort)},
			{Name: "LINKERD2_PROXY_METRICS_LISTENER", Value: fmt.Sprintf("tcp://0.0.0.0:%d", options.proxyMetricsPort)},
			{Name: "LINKERD2_PROXY_OUTBOUND_LISTENER", Value: fmt.Sprintf("tcp://127.0.0.1:%d", options.outboundPort)},
			{Name: "LINKERD2_PROXY_INBOUND_LISTENER", Value: fmt.Sprintf("tcp://0.0.0.0:%d", options.inboundPort)},
			{Name: "LINKERD2_PROXY_DESTINATION_PROFILE_SUFFIXES", Value: profileSuffixes},
			{Name: "LINKERD2_PROXY_INBOUND_ACCEPT_KEEPALIVE", Value: fmt.Sprintf("%dms", defaultKeepaliveMs)},
			{Name: "LINKERD2_PROXY_OUTBOUND_CONNECT_KEEPALIVE", Value: fmt.Sprintf("%dms", defaultKeepaliveMs)},
			{
				Name:      "K8S_SA",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.serviceAccountName"}},
			},
			{
				Name:      "K8S_NS",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
			},
			{Name: "L5D_NS", Value: controlPlaneNamespace},
			{Name: "L5D_TRUST_DOMAIN", Value: "cluster.local"},
			{Name: "LINKERD2_PROXY_ID", Value: fmt.Sprintf("$(K8S_SA).$(K8S_NS).serviceaccount.identity.$(L5D_NS).$(L5D_TRUST_DOMAIN)")},
		},
		LivenessProbe:  &proxyProbe,
		ReadinessProbe: &proxyProbe,
	}

	// Special case if the caller specifies that
	// LINKERD2_PROXY_OUTBOUND_ROUTER_CAPACITY be set on the pod.
	// We key off of any container image in the pod. Ideally we would instead key
	// off of something at the top-level of the PodSpec, but there is nothing
	// easily identifiable at that level.
	// This is currently only used by the Prometheus pod in the control-plane.
	for _, container := range t.Containers {
		if capacity, ok := options.proxyOutboundCapacity[container.Image]; ok {
			sidecar.Env = append(sidecar.Env,
				corev1.EnvVar{
					Name:  "LINKERD2_PROXY_OUTBOUND_ROUTER_CAPACITY",
					Value: fmt.Sprintf("%d", capacity),
				},
			)
			break
		}
	}

	if options.enableTLS() {
		base := "/var/run/linkerd/identity"
		anchors := base + "/trust-anchors/trust-anchors.pem"
		endEntity := base + "/end-entity"
		tlsEnvVars := []corev1.EnvVar{
			{Name: "LINKERD2_PROXY_TLS_TRUST_ANCHORS", Value: anchors},
			{Name: "LINKERD2_PROXY_TLS_END_ENTITY_DIR", Value: endEntity},
			{Name: "LINKERD2_PROXY_TLS_POD_IDENTITY", Value: "$(LINKERD2_PROXY_ID)"},
			{Name: "LINKERD2_PROXY_CONTROLLER_NAMESPACE", Value: controlPlaneNamespace},
			{
				Name: "LINKERD2_PROXY_TLS_CONTROLLER_IDENTITY",
				// FIXME
				Value: fmt.Sprintf("linkerd-controller.%s.serviceaccount.identity.%s.cluster.local", controlPlaneNamespace, controlPlaneNamespace),
			},
		}
		sidecar.Env = append(sidecar.Env, tlsEnvVars...)

		volume := corev1.Volume{
			Name: "linkerd-identity-end-entity",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: "Memory",
				},
			},
		}
		sidecar.VolumeMounts = []corev1.VolumeMount{
			{Name: volume.Name, MountPath: endEntity, ReadOnly: true},
		}
		t.Volumes = append(t.Volumes, volume)
	}

	t.Containers = append(t.Containers, sidecar)
	if !options.noInitContainer {
		nonRoot := false
		runAsUser := int64(0)
		initContainer := corev1.Container{
			Name:                     k8s.InitContainerName,
			Image:                    options.taggedProxyInitImage(),
			ImagePullPolicy:          corev1.PullPolicy(options.imagePullPolicy),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			Args:                     initArgs,
			SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{
					Add: []corev1.Capability{corev1.Capability("NET_ADMIN")},
				},
				Privileged:   &f,
				RunAsNonRoot: &nonRoot,
				RunAsUser:    &runAsUser,
			},
		}
		t.InitContainers = append(t.InitContainers, initContainer)
	}
}

func (rt resourceTransformerInject) transform(bytes []byte, options *injectOptions) ([]byte, []injectReport, error) {
	conf := &resourceConfig{}
	output, reports, err := conf.parse(bytes, options, rt)
	if output != nil || err != nil {
		return output, reports, err
	}

	report := injectReport{
		kind: strings.ToLower(conf.meta.Kind),
		name: conf.om.Name,
	}

	// If we don't inject anything into the pod template then output the
	// original serialization of the original object. Otherwise, output the
	// serialization of the modified object.
	output = bytes
	if conf.podSpec != nil {
		metaAccessor, err := k8sMeta.Accessor(conf.obj)
		if err != nil {
			return nil, nil, err
		}

		// The namespace isn't necessarily in the input so it has to be substituted
		// at runtime. The proxy recognizes the "$NAME" syntax for this variable
		// but not necessarily other variables.
		identity := k8s.TLSIdentity{
			Name:                metaAccessor.GetName(),
			Kind:                strings.ToLower(conf.meta.Kind),
			ControllerNamespace: controlPlaneNamespace,
		}

		report.update(conf.objectMeta, conf.podSpec)
		if report.shouldInject() {
			injectObjectMeta(conf.objectMeta, conf.k8sLabels, options)
			injectPodSpec(conf.podSpec, identity, conf.dnsNameOverride, options)

			var err error
			output, err = yaml.Marshal(conf.obj)
			if err != nil {
				return nil, nil, err
			}
		}
	} else {
		report.unsupportedResource = true
	}

	return output, []injectReport{report}, nil
}

func (resourceTransformerInject) generateReport(injectReports []injectReport, output io.Writer) {
	injected := []injectReport{}
	hostNetwork := []string{}
	sidecar := []string{}
	udp := []string{}
	injectDisabled := []string{}
	warningsPrinted := verbose

	for _, r := range injectReports {
		if !r.hostNetwork && !r.sidecar && !r.unsupportedResource && !r.injectDisabled {
			injected = append(injected, r)
		}

		if r.hostNetwork {
			hostNetwork = append(hostNetwork, r.resName())
			warningsPrinted = true
		}

		if r.sidecar {
			sidecar = append(sidecar, r.resName())
			warningsPrinted = true
		}

		if r.udp {
			udp = append(udp, r.resName())
			warningsPrinted = true
		}

		if r.injectDisabled {
			injectDisabled = append(injectDisabled, r.resName())
			warningsPrinted = true
		}
	}

	//
	// Warnings
	//

	// Leading newline to separate from yaml output on stdout
	output.Write([]byte("\n"))

	if len(hostNetwork) > 0 {
		output.Write([]byte(fmt.Sprintf("%s \"hostNetwork: true\" detected in %s\n", warnStatus, strings.Join(hostNetwork, ", "))))
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, hostNetworkDesc)))
	}

	if len(sidecar) > 0 {
		output.Write([]byte(fmt.Sprintf("%s known 3rd party sidecar detected in %s\n", warnStatus, strings.Join(sidecar, ", "))))
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, sidecarDesc)))
	}

	if len(injectDisabled) > 0 {
		output.Write([]byte(fmt.Sprintf("%s \"%s: %s\" annotation set on %s\n",
			warnStatus, k8s.ProxyInjectAnnotation, k8s.ProxyInjectDisabled, strings.Join(injectDisabled, ", "))))
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, injectDisabledDesc)))
	}

	if len(injected) == 0 {
		output.Write([]byte(fmt.Sprintf("%s no supported objects found\n", warnStatus)))
		warningsPrinted = true
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, unsupportedDesc)))
	}

	if len(udp) > 0 {
		verb := "uses"
		if len(udp) > 1 {
			verb = "use"
		}
		output.Write([]byte(fmt.Sprintf("%s %s %s \"protocol: UDP\"\n", warnStatus, strings.Join(udp, ", "), verb)))
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, udpDesc)))
	}

	//
	// Summary
	//
	if warningsPrinted {
		output.Write([]byte("\n"))
	}

	for _, r := range injectReports {
		if !r.hostNetwork && !r.sidecar && !r.unsupportedResource && !r.injectDisabled {
			output.Write([]byte(fmt.Sprintf("%s \"%s\" injected\n", r.kind, r.name)))
		} else {
			if r.kind != "" {
				output.Write([]byte(fmt.Sprintf("%s \"%s\" skipped\n", r.kind, r.name)))
			} else {
				output.Write([]byte(fmt.Sprintf("document missing \"kind\" field, skipped\n")))
			}
		}
	}

	// Trailing newline to separate from kubectl output if piping
	output.Write([]byte("\n"))
}
