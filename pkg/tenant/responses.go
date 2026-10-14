package tenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	responsesSecretName         = "responses-db-config"
	responsesCAName             = "responses-db-ca"
	responsesOwnerLabel         = "ai-gateway-controller.opendatahub.io/responses-tenant-uid"
	responsesIdentityAnnotation = "ai-gateway-controller.opendatahub.io/responses-database-identity"
	responsesBindingDir         = "/etc/praxis/responses-binding"
	responsesKeyDir             = "/var/run/praxis/responses"
)

type responsesBinding struct {
	resourceVersion string
	tenantUID       string
	secret          *corev1.Secret
}

// Read platform credentials directly, never through the informer cache.
func (r *Reconciler) responsesBinding(ctx context.Context, owner *unstructured.Unstructured, namespace string) (*responsesBinding, error) {
	enabled, _, err := unstructured.NestedBool(owner.Object, "spec", "responses", "enabled")
	if err != nil {
		return nil, errors.New("responses.enabled must be a boolean")
	}
	if !enabled {
		return nil, nil
	}
	for field, allowed := range map[string]string{"mode": "PlatformDefault", "deletionPolicy": "Retain"} {
		value, found, err := unstructured.NestedString(owner.Object, "spec", "responses", "storage", field)
		if err != nil || (found && value != allowed) {
			return nil, fmt.Errorf("responses.storage.%s must be %s", field, allowed)
		}
	}
	if _, found, err := unstructured.NestedFieldNoCopy(owner.Object, "spec", "responses", "retention", "maxAge"); err != nil || found {
		return nil, errors.New("responses retention.maxAge is not implemented")
	}
	if r.APIReader == nil || r.InfrastructureNamespace == "" || owner.GetUID() == "" {
		return nil, errors.New("responses requires an API reader, infrastructure namespace and tenant UID")
	}
	connection, ca := &corev1.Secret{}, &corev1.Secret{}
	for name, secret := range map[string]*corev1.Secret{responsesSecretName: connection, responsesCAName: ca} {
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.InfrastructureNamespace, Name: name}, secret); err != nil {
			return nil, fmt.Errorf("responses platform binding %s is unavailable", name)
		}
	}
	data := map[string][]byte{"ca.crt": ca.Data["ca.crt"]}
	for _, key := range []string{"DB_CONNECTION_URL", "tls.crt", "tls.key"} {
		data[key] = connection.Data[key]
	}
	for key, value := range data {
		if len(value) == 0 {
			return nil, fmt.Errorf("responses platform binding is missing %s", key)
		}
	}
	identity, err := responsesDatabaseIdentity(data["DB_CONNECTION_URL"])
	if err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(strings.TrimSpace(string(data["DB_CONNECTION_URL"])))
	if _, password := parsed.User.Password(); password {
		return nil, errors.New("responses requires a password-free certificate binding")
	}
	maas := &corev1.Secret{}
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.InfrastructureNamespace, Name: "maas-db-config"}, maas)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, errors.New("cannot check MaaS database separation")
	}
	if err == nil {
		maasIdentity, identityErr := responsesDatabaseIdentity(maas.Data["DB_CONNECTION_URL"])
		if identityErr != nil {
			return nil, errors.New("cannot validate MaaS database separation")
		}
		if identity == maasIdentity {
			return nil, errors.New("responses must not reuse the MaaS API-key database")
		}
	}
	revision := fmt.Sprintf("%s/%s/%s/%s/%s", owner.GetUID(), connection.UID, connection.ResourceVersion, ca.UID, ca.ResourceVersion)
	hash := sha256.Sum256([]byte(revision))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: responsesRuntimeSecretName(owner.GetName()), Namespace: namespace,
			Labels:      map[string]string{LabelManagedBy: ManagedByAIGatewayController, responsesOwnerLabel: string(owner.GetUID())},
			Annotations: map[string]string{responsesIdentityAnnotation: identity},
		},
		Type: corev1.SecretTypeOpaque, Data: data,
	}
	if err := r.checkResponsesProjection(ctx, secret); err != nil {
		return nil, err
	}
	return &responsesBinding{resourceVersion: hex.EncodeToString(hash[:]), tenantUID: string(owner.GetUID()), secret: secret}, nil
}

func responsesRuntimeSecretName(tenantID string) string {
	if tenantID == DefaultAITenantName {
		tenantID = ""
	}
	return ResourceName("responses-runtime", tenantID)
}

// Hash only endpoint/database identity; never record a URL or its credentials.
func responsesDatabaseIdentity(raw []byte) (string, error) {
	u, err := url.Parse(strings.TrimSpace(string(raw)))
	if err != nil || (u.Scheme != "postgresql" && u.Scheme != "postgres") || u.Hostname() == "" || u.User == nil || u.User.Username() == "" || strings.TrimPrefix(u.Path, "/") == "" || u.Fragment != "" {
		return "", errors.New("invalid PostgreSQL database binding")
	}
	// Endpoint overrides would make the URL identity misleading.
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errors.New("invalid PostgreSQL database binding query")
	}
	for key := range query {
		if key != "sslmode" {
			return "", errors.New("database binding cannot override connection parameters")
		}
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	hash := sha256.Sum256([]byte(net.JoinHostPort(host, port) + "/" + strings.TrimPrefix(u.Path, "/")))
	return hex.EncodeToString(hash[:]), nil
}

func (r *Reconciler) checkResponsesProjection(ctx context.Context, desired *corev1.Secret) error {
	current := &corev1.Secret{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errors.New("cannot read responses runtime binding")
	}
	if current.Labels[LabelManagedBy] != ManagedByAIGatewayController || current.Labels[responsesOwnerLabel] != desired.Labels[responsesOwnerLabel] {
		return errors.New("responses runtime binding has foreign ownership")
	}
	if current.Annotations[responsesIdentityAnnotation] != desired.Annotations[responsesIdentityAnnotation] {
		return errors.New("responses database change requires an explicit cutover")
	}
	desired.ResourceVersion = current.ResourceVersion
	desired.UID = current.UID
	return nil
}

// Use optimistic concurrency, not force-apply, for private credentials.
func (r *Reconciler) projectResponsesBinding(ctx context.Context, binding *responsesBinding) error {
	if err := r.checkResponsesProjection(ctx, binding.secret); err != nil {
		return err
	}
	if binding.secret.ResourceVersion == "" {
		if err := r.Client.Create(ctx, binding.secret); err != nil {
			return errors.New("cannot create responses runtime binding")
		}
		return nil
	}
	current := &corev1.Secret{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(binding.secret), current); err != nil {
		return errors.New("cannot read responses runtime binding")
	}
	if reflect.DeepEqual(current.Data, binding.secret.Data) && reflect.DeepEqual(current.Labels, binding.secret.Labels) && reflect.DeepEqual(current.Annotations, binding.secret.Annotations) {
		return nil
	}
	if err := r.Client.Update(ctx, binding.secret); err != nil {
		return errors.New("cannot update responses runtime binding")
	}
	return nil
}

func (r *Reconciler) cleanupResponsesProjection(ctx context.Context, tenantID, namespace string) error {
	secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: responsesRuntimeSecretName(tenantID)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errors.New("cannot read responses runtime binding for cleanup")
	}
	if secret.Labels[LabelManagedBy] != ManagedByAIGatewayController || secret.Labels[responsesOwnerLabel] == "" {
		return nil
	}
	if err := client.IgnoreNotFound(r.Client.Delete(ctx, secret, client.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion})); err != nil {
		return errors.New("cannot delete responses runtime binding")
	}
	return nil
}

// Flat conditions also execute body hooks; pinned core branches run head hooks only.
func writeResponsesFilters(b *strings.Builder, tenantUID string) {
	b.WriteString("      - filter: ai_operation\n        headers: {application_protocol: null, operation: null}\n")
	for _, api := range []string{"responses", "conversations"} {
		writeResponsesFilter(b, "state_owner", api)
		b.WriteString("        mode: trusted_headers\n")
		fmt.Fprintf(b, "        tenant: {static: %q}\n", tenantUID)
		b.WriteString("        issuer: {static: \"urn:maas:authorino\"}\n        subject: {header: X-MaaS-Username}\n")
		if api == "responses" {
			writeResponsesFilter(b, "openai_responses_request", api)
			writeResponsesStore(b, "openai_responses_store", api)
			b.WriteString("        responses_table: openai_responses\n        conversations_table: openai_conversations\n")
			writeResponsesFilter(b, "openai_responses_rehydrate", api)
			writeResponsesFilter(b, "openai_responses_proxy", api)
		}
		// Conditions are ANDed; state operations and append-back need separate entries.
		writeResponsesStore(b, "openai_conversations", api)
		b.WriteString("        conversations_table: openai_conversations\n        items_table: openai_conversation_items\n")
	}
}

func writeResponsesFilter(b *strings.Builder, filter, api string) {
	fmt.Fprintf(b, "      - filter: %s\n        conditions: [{when: {path_prefix: /v1/%s}}]\n", filter, api)
}

func writeResponsesStore(b *strings.Builder, filter, api string) {
	writeResponsesFilter(b, filter, api)
	b.WriteString("        backend: postgres\n")
	fmt.Fprintf(b, "        database_url_file: %s/DB_CONNECTION_URL\n", responsesBindingDir)
	fmt.Fprintf(b, "        ssl_root_cert: %s/ca.crt\n        ssl_client_cert: %s/tls.crt\n", responsesBindingDir, responsesBindingDir)
	fmt.Fprintf(b, "        ssl_client_key: %s/tls.key\n", responsesKeyDir)
	b.WriteString("        ssl_mode: verify-full\n        require_certificate_authentication: true\n        allow_private_database_url: true\n")
	b.WriteString("        pool: {max_connections: 2, acquire_timeout_secs: 10}\n")
}

func mountResponsesBinding(deployment *unstructured.Unstructured, binding responsesBinding) error {
	if err := unstructured.SetNestedField(deployment.Object, binding.tenantUID, "spec", "template", "metadata", "labels", responsesOwnerLabel); err != nil {
		return err
	}
	containers, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		return errors.New("responses ExtProc containers are missing")
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return errors.New("responses ExtProc container is malformed")
	}
	image, _, _ := unstructured.NestedString(container, "image")
	if image == "" {
		return errors.New("responses ExtProc image is missing")
	}
	mounts, _, err := unstructured.NestedSlice(container, "volumeMounts")
	if err != nil {
		return err
	}
	bindingMount := corev1.VolumeMount{Name: "responses-binding", MountPath: responsesBindingDir, ReadOnly: true}
	keyMount := corev1.VolumeMount{Name: "responses-client-key", MountPath: responsesKeyDir, ReadOnly: true}
	for _, mount := range []corev1.VolumeMount{bindingMount, keyMount} {
		mounts, err = appendResponsesNamedObject(mounts, mount.Name, &mount)
		if err != nil {
			return err
		}
	}
	container["volumeMounts"] = mounts
	// TCP liveness is intentional; readiness must check the named gRPC service.
	probe := &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 50052, Service: ptr.To("envoy.service.ext_proc.v3.ExternalProcessor")}},
		InitialDelaySeconds: 5, PeriodSeconds: 10,
	}
	container["readinessProbe"], err = runtime.DefaultUnstructuredConverter.ToUnstructured(probe)
	if err != nil {
		return err
	}
	containers[0] = container
	if err := unstructured.SetNestedSlice(deployment.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		return err
	}
	return mountResponsesVolumes(deployment, image, bindingMount, keyMount, binding.secret.Name, binding.resourceVersion)
}

func mountResponsesVolumes(deployment *unstructured.Unstructured, image string, bindingMount, keyMount corev1.VolumeMount, secretName, revision string) error {
	volumes, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return err
	}
	items := make([]corev1.KeyToPath, 0, 4)
	for _, key := range []string{"DB_CONNECTION_URL", "ca.crt", "tls.crt", "tls.key"} {
		items = append(items, corev1.KeyToPath{Key: key, Path: key})
	}
	for _, volume := range []corev1.Volume{
		{Name: "responses-binding", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName, DefaultMode: ptr.To[int32](0444), Items: items}}},
		{Name: "responses-client-key", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("1Mi"))}}},
	} {
		volumes, err = appendResponsesNamedObject(volumes, volume.Name, &volume)
		if err != nil {
			return err
		}
	}
	if err := unstructured.SetNestedSlice(deployment.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		return err
	}
	// Secret files are root-owned. Copy once as the same assigned UID as ExtProc.
	keyMount.ReadOnly = false
	init := &corev1.Container{
		Name: "responses-client-key", Image: image,
		Command: []string{"/bin/sh", "-ec", "umask 077; rm -f " + responsesKeyDir + "/tls.key; cat " + responsesBindingDir + "/tls.key > " + responsesKeyDir + "/tls.key; test -s " + responsesKeyDir + "/tls.key"},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), RunAsNonRoot: ptr.To(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{bindingMount, keyMount},
	}
	initContainers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "initContainers")
	if err != nil {
		return err
	}
	initContainers, err = appendResponsesNamedObject(initContainers, init.Name, init)
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedSlice(deployment.Object, initContainers, "spec", "template", "spec", "initContainers"); err != nil {
		return err
	}
	return unstructured.SetNestedField(deployment.Object, revision, "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/responses-binding-revision")
}

func appendResponsesNamedObject(items []any, name string, object any) ([]any, error) {
	value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, err
	}
	return appendOrReplaceNamedVolume(items, name, value), nil
}
