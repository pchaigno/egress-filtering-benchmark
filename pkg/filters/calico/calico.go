package calico

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"text/template"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

const (
	rulesPerManifest = 10000
)

type calicoCNI struct {
	Iface                     string
	Nets                      []string
	GlobalNetworkSetManifests []string
}

func New(nets []net.IPNet, iface string) *calicoCNI {
	netsList := make([]string, len(nets))
	for i, n := range nets {
		netsList[i] = n.String()
	}

	return &calicoCNI{
		Iface:                     iface,
		Nets:                      netsList,
		GlobalNetworkSetManifests: make([]string, 0),
	}
}

// SetUp installs the filter in iface
func (b *calicoCNI) SetUp(nets []net.IPNet, iface string) (int64, error) {
	start := time.Now()

	gnsManifests := map[int][]string{}

	// This code splits the GlobalNetworkSet manifests to accomodate only
	// 10000 entries per manifest.
	current := 0
	maxManifests := (len(b.Nets) / rulesPerManifest) + 1

	for i := 0; i < maxManifests; i++ {
		remaining := len(b.Nets) - current
		if remaining >= rulesPerManifest {
			gnsManifests[i] = b.Nets[current : current+rulesPerManifest]
		} else {
			gnsManifests[i] = b.Nets[current : current+remaining]
		}

		current = current + rulesPerManifest

		m := struct {
			Index int
			Nets  []string
		}{
			Index: i,
			Nets:  gnsManifests[i],
		}

		rendered, err := renderTemplate(globalNetworkSetTmpl, m)
		if err != nil {
			return 0, fmt.Errorf("rendering GlobalNetworkSet template: %w", err)
		}

		b.GlobalNetworkSetManifests = append(b.GlobalNetworkSetManifests, rendered)
	}
	// Render GlobalNetworkPolicy
	gnpManifest, err := renderTemplate(globalNetworkPolicyTmpl, b)
	if err != nil {
		return 0, fmt.Errorf("rendering GlobalNetworkPolicy template: %w", err)
	}
	// Render GlobalNetworkPolicy
	gnpWorkloadsManifest, err := renderTemplate(gnpTmplForWorkloads, b)
	if err != nil {
		return 0, fmt.Errorf("rendering GlobalNetworkPolicy workloads template: %w", err)
	}

	// Get in cluster config, to create k8s resources.
	config, err := rest.InClusterConfig()
	if err != nil {
		return 0, err
	}

	for _, gnsManifest := range b.GlobalNetworkSetManifests {
		// Decode the apply the GlobalNetworkSet manifest
		if err := decodeAndApply(config, gnsManifest, "CREATE"); err != nil {
			return 0, err
		}
	}

	// Decode the apply the GlobalNetworkPolicy manifest
	if err := decodeAndApply(config, gnpManifest, "CREATE"); err != nil {
		return 0, err
	}
	// Decode the apply the GlobalNetworkPolicy manifest
	if err := decodeAndApply(config, gnpWorkloadsManifest, "CREATE"); err != nil {
		return 0, err
	}

	elapsed := time.Since(start)

	return elapsed.Nanoseconds(), nil
}

func renderTemplate(tmpl string, obj interface{}) (string, error) {
	t, err := template.New("render").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err = t.Execute(&buf, obj); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func decodeAndApply(config *rest.Config, data string, action string) error {
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	dd, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("creating dynamic config: %w", err)
	}

	r := strings.NewReader(data)
	yamlReader := yamlutil.NewYAMLReader(bufio.NewReader(r))
	b, err := yamlReader.Read()
	if err != nil {
		return fmt.Errorf("reading yaml: %w", err)
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(b), len(b))
	var rawObj runtime.RawExtension
	if err := decoder.Decode(&rawObj); err != nil {
		return fmt.Errorf("decoding to raw object: %w", err)
	}

	obj, gvk, err := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme).Decode(rawObj.Raw, nil, nil)
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Errorf("serializing to unstructured: %w", err)
	}

	unstructuredObj := &unstructured.Unstructured{Object: unstructuredMap}

	gr, err := restmapper.GetAPIGroupResources(clientset.Discovery())
	if err != nil {
		return fmt.Errorf("getting APIGroupResources: %w", err)
	}

	mapper := restmapper.NewDiscoveryRESTMapper(gr)
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("rest mapping: %w", err)
	}

	var dri dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if unstructuredObj.GetNamespace() == "" {
			unstructuredObj.SetNamespace("default")
		}
		dri = dd.Resource(mapping.Resource).Namespace(unstructuredObj.GetNamespace())
	} else {
		dri = dd.Resource(mapping.Resource)
	}

	if action == "CREATE" {
		if _, err := dri.Create(context.Background(), unstructuredObj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating resource %s: %w", unstructuredObj.GetName(), err)
		}
	}

	if action == "DELETE" {
		if err := dri.Delete(context.Background(), unstructuredObj.GetName(), metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("deleting resource %s: %w", unstructuredObj.GetName(), err)
		}
	}

	return nil
}

func (b *calicoCNI) CleanUp() {
	gnpManifest, err := renderTemplate(globalNetworkPolicyTmpl, b)
	if err != nil {
		fmt.Printf("rendering GlobalNetworkPolicy template: %w", err)
	}

	gnpWorkloadsManifest, err := renderTemplate(gnpTmplForWorkloads, b)
	if err != nil {
		fmt.Printf("rendering GlobalNetworkPolicy workloads template: %w", err)
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Printf("creating cluster config: %w", err)
	}

	for _, gnsManifest := range b.GlobalNetworkSetManifests {
		// Decode the apply the GlobalNetworkSet manifest
		if err := decodeAndApply(config, gnsManifest, "DELETE"); err != nil {
			fmt.Printf("deleting GlobalNetworkSets: %w", err)
		}
	}
	// Decode the apply the GlobalNetworkPolicy manifest
	if err := decodeAndApply(config, gnpManifest, "DELETE"); err != nil {
		fmt.Printf("deleting GlobalNetworkPolicy: %w", err)
	}
	// Decode the apply the GlobalNetworkPolicy manifest
	if err := decodeAndApply(config, gnpWorkloadsManifest, "DELETE"); err != nil {
		fmt.Printf("deleting GlobalNetworkPolicy: %w", err)
	}
}
