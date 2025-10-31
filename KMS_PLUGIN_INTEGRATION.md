# KMS Plugin Integration for kube-apiserver-operator

This document describes the KMS plugin sidecar integration into the kube-apiserver operator.

## ✅ Completed Integration

### Files Modified

1. **`pkg/operator/targetconfigcontroller/targetconfigcontroller.go`**
   - Added `kmsPluginImage` field to `TargetConfigController` struct
   - Added `apiserverLister` field for reading cluster APIServer config
   - Updated `NewTargetConfigController()` to accept `kmsPluginImage` and `apiserverInformer` parameters
   - Updated `managePods()` to call KMS plugin injection
   - Added APIServer informer to the controller's watch list

2. **`pkg/operator/starter.go`**
   - Updated call to `NewTargetConfigController()` with new parameters:
     - `os.Getenv("KMS_PLUGIN_IMAGE")` for the plugin image
     - `configInformers.Config().V1().APIServers()` for the APIServer informer

### Files Created

3. **`pkg/operator/targetconfigcontroller/kms_plugin.go`**
   - `getKMSEncryptionConfig()` - Reads APIServer config to check if KMS is enabled
   - `injectKMSPlugin()` - Main integration function that injects the KMS sidecar

### Build Configuration

4. **`go.mod`**
   - Added replace directive: `replace github.com/openshift/library-go => ../library-go`
   - This allows using the local library-go with the new kmsplugin package

## How It Works

### 1. Configuration Detection

The operator reads the cluster-scoped APIServer config (`config.openshift.io/v1`):

```yaml
apiVersion: config.openshift.io/v1
kind: APIServer
metadata:
  name: cluster
spec:
  encryption:
    type: KMS
    kms:
      aws:
        keyARN: arn:aws:kms:us-east-1:123456789012:key/...
        region: us-east-1
```

If `spec.encryption.type == "KMS"`, the operator injects the KMS plugin sidecar.

### 2. Sidecar Injection Flow

```
TargetConfigController.sync()
  ↓
createTargetConfig()
  ↓
managePods()
  ↓
injectKMSPlugin()  ← New function
  ↓
kmsplugin.AddKMSPluginToPodSpec()  ← From library-go
  ↓
Pod spec updated with:
  - KMS plugin container
  - Socket volume (hostPath)
  - Socket mount in kube-apiserver container
```

### 3. Credential Management

**kube-apiserver uses `hostNetwork: true`**, so:
- KMS plugin accesses AWS via EC2 Instance Metadata Service (IMDS)
- Automatically uses the master node's IAM role
- **No CredentialsRequest or secret needed**

Prerequisites:
- Master node IAM role must have KMS permissions
- Use the `master-node-iam-setup.sh` script from library-go to configure this

### 4. Controller Reactivity

The operator watches the APIServer resource and automatically resyncs when:
- APIServer config changes (encryption type, KMS config)
- KMS plugin image changes (via `KMS_PLUGIN_IMAGE` env var)

## Testing the Integration

### 0. Update the Operator

Follow the steps below if your cluster isn't already running with the changes in this PR:

```bash
# make sure you're logged in to registry.ci.openshift.org, use your token from https://console-openshift-console.apps.ci.l2s4.p1.openshiftapps.com/topology/ns/ocp?view=graph
podman login -u=unused registry.ci.openshift.org

# now build the operator image
quay_user=fmissi
podman build -f Dockerfile.rhel7 -t quay.io/${quay_user}/cluster-kube-apiserver-operator:kms-dev .
podman push --digestfile /tmp/digest.txt quay.io/${quay_user}/cluster-kube-apiserver-operator:kms-dev


# stop cvo from interfering
oc scale --replicas=0 deployment/cluster-version-operator -n openshift-cluster-version
oc get deployment cluster-version-operator -n openshift-cluster-version

# update the operator image
image="quay.io/${quay_user}/cluster-kube-apiserver-operator@$(cat /tmp/digest.txt)"
oc patch deployment kube-apiserver-operator \
    -n openshift-kube-apiserver-operator \
    --type=json \
    -p='[{
      "op": "replace",
      "path": "/spec/template/spec/containers/0/image",
      "value": "'"$image"'"
    }]'

# verify the new image is deployed (might take a moment)
oc get deployment kube-apiserver-operator \
    -n openshift-kube-apiserver-operator \
    -o jsonpath='{.spec.template.spec.containers[0].image}'
```

### 1. Set Environment Variables

**Important**: The `KMS_PLUGIN_IMAGE` environment variable must be set on the **kube-apiserver-operator** deployment (not on the kube-apiserver static pod itself). The operator reads this variable and uses it when generating the static pod manifest.

Patch the kube-apiserver-operator deployment to add the KMS plugin image:

```bash
oc patch deployment kube-apiserver-operator \
  -n openshift-kube-apiserver-operator \
  --type=json \
  -p='[{
    "op": "add",
    "path": "/spec/template/spec/containers/0/env/-",
    "value": {
      "name": "KMS_PLUGIN_IMAGE",
      "value": "quay.io/fmissi/aws-kms-plugin:0.1.0"
    }
  }]'
```

This will be used by operator later on, after we enable encryption.

### 2. Configure Master Node IAM Role

Run the helper script:
```bash
# make sure you have checked out the kms-plugin-sidecars from github.com/flavianmissi/library-go
cd $(go env GOPATH)src/github.com/openshift/library-go/pkg/operator/encryption/kms
./master-node-iam-setup.sh
```

This adds KMS permissions to the master node IAM role.

### 3. Enable KMS Encryption

Create/update the APIServer config:

```bash
key_arn='arn:aws:kms:us-east-2:123456:key/123456-123456-123-123-12345678'
key_region='us-east-2'
oc patch apiserver cluster --type=merge --patch="
spec:
  encryption:
    type: KMS
    kms:
      type: AWS
      aws:
        keyARN: ${key_arn}
        region: ${key_region}
"

# verify
oc get apiserver/cluster -ojsonpath="{.spec.encryption}"
```

### 4. Verify Injection

Expected output should show the kms-plugin container definition with your specified image.

Check the kube-apiserver pod configmap:

```bash
revision=$(oc get pods -n=openshift-kube-apiserver -l app=openshift-kube-apiserver -ojsonpath='{.items[0].metadata.labels.revision}')
oc get configmap -n openshift-kube-apiserver "kube-apiserver-pod-${revision}" -o jsonpath='{.data.pod\.yaml}' | grep -A 10 "name: kms-plugin"
```

Check static pod on a master node:

```bash
# SSH to a master node
oc debug node/master-0

# Check the pod manifest
chroot /host cat /etc/kubernetes/manifests/kube-apiserver-pod.yaml | grep -A20 "name: kms-plugin"
```

### 5. Verify KMS Plugin is Running

```bash
# On a master node
oc debug node/master-0
chroot /host

# Check if kms-plugin container is running
crictl pods | grep kube-apiserver
crictl ps | grep kms-plugin

# Check logs
crictl logs <container-id>
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ Master Node (hostNetwork: true)                             │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌────────────────┐         ┌──────────────────┐           │
│  │ kube-apiserver │ ◄─────► │   kms-plugin     │           │
│  │                │  Unix   │                  │           │
│  │                │  Socket │                  │           │
│  └────────────────┘         └──────────────────┘           │
│                                     │                       │
│                                     │ IMDS                  │
│                                     ▼                       │
│                             169.254.169.254                 │
│                             (Master IAM Role)               │
│                                     │                       │
└──────────────────────────────────────┼──────────────────────┘
                                       │
                                       ▼
                                   AWS KMS
```

## Next Steps

1. Build and deploy the operator with these changes
2. Test on a real AWS-based OpenShift cluster
3. Verify encryption/decryption works
4. Apply similar pattern to openshift-apiserver and oauth-apiserver (with credential secrets)

## Production Considerations

### Image Management

Currently using `KMS_PLUGIN_IMAGE` env var. For production:
- Include KMS plugin image in operator's release payload
- Reference it like `OPERATOR_IMAGE` is referenced

### Error Handling

The current implementation will:
- Log errors if KMS config is invalid
- Fail pod creation if injection fails
- This prevents misconfigured KMS from silently failing

### Monitoring

Add metrics/alerts for:
- KMS plugin health
- Encryption/decryption success/failure rates
- AWS KMS API call latency

## Related Documentation

- Shared library: `/path/to/library-go/pkg/operator/encryption/kmsplugin/README.md`
- CredentialsRequest setup: `/path/to/library-go/pkg/operator/encryption/kmsplugin/*.sh`
- OpenShift API types: `github.com/openshift/api/config/v1/types_kmsencryption.go`
