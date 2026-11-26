#!/usr/bin/env python3
"""Reproduce startup config/Secret skew with a selected Praxis runtime image."""

import argparse
import hashlib
import json
import os
import shlex
import subprocess
import time
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--timeout", type=int, default=120)
    args = parser.parse_args()
    if not args.context.strip() or not args.image.strip():
        parser.error("context and image must be non-empty")
    namespace = f"agc-config-snapshot-{uuid.uuid4().hex[:8]}"
    annotation = "ai-gateway-controller.opendatahub.io/extproc-config"

    def kubectl(*command, document=None):
        result = subprocess.run(
            [
                *shlex.split(os.environ.get("KUBECTL", "kubectl")),
                "--context",
                args.context,
                *command,
            ],
            input=json.dumps(document) if document is not None else "",
            text=True,
            capture_output=True,
            timeout=args.timeout + 10,
        )
        if result.returncode:
            raise RuntimeError(f"kubectl {' '.join(command)}: {result.stderr}")
        return result.stdout

    def config(key):
        return f"""server:
  grpc_address: "0.0.0.0:9004"
  health_address: "0.0.0.0:50052"
  metrics_address: "0.0.0.0:9090"
  tls:
    mode: self_signed
filter_chains:
  - name: post-auth
    filters:
      - filter: credential_inject
        credentials:
          - name: credential-{key}
            namespace: {namespace}
            key: api-key
            strategy: bearer_token
            file: /etc/praxis/credentials/credential-{key}/api-key
insecure_options:
  allow_unbounded_body: true
"""

    def secret(key, token):
        return {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {
                "name": f"credential-{key}",
                "namespace": namespace,
            },
            "stringData": {"api-key": token},
        }

    objects = []
    for key in ("a", "b"):
        objects.extend(
            [
                secret(key, f"synthetic-test-token-{key}"),
                {
                    "apiVersion": "v1",
                    "kind": "ConfigMap",
                    "metadata": {
                        "name": f"startup-{key}",
                        "namespace": namespace,
                    },
                    "data": {"extproc.yaml": config(key)},
                },
            ]
        )
    for key, cached in (("a", "b"), ("b", "a")):
        for mode in ("baseline", "fixed"):
            source = (
                {"configMap": {"name": f"startup-{cached}"}}
                if mode == "baseline"
                else {
                    "downwardAPI": {
                        "items": [
                            {
                                "path": "extproc.yaml",
                                "fieldRef": {
                                    "apiVersion": "v1",
                                    "fieldPath": f"metadata.annotations['{annotation}']",
                                },
                            }
                        ]
                    },
                }
            )
            objects.append(
                {
                    "apiVersion": "v1",
                    "kind": "Pod",
                    "metadata": {
                        "name": f"{mode}-{key}",
                        "namespace": namespace,
                        "labels": {"case": mode},
                        "annotations": {annotation: config(key)},
                    },
                    "spec": {
                        "containers": [
                            {
                                "name": "processor",
                                "image": args.image,
                                "imagePullPolicy": "IfNotPresent",
                                "args": ["-c", "/etc/praxis/extproc.yaml"],
                                "resources": {
                                    "requests": {"cpu": "50m", "memory": "64Mi"},
                                    "limits": {"cpu": "250m", "memory": "256Mi"},
                                },
                                "readinessProbe": {
                                    "grpc": {
                                        "port": 50052,
                                        "service": "envoy.service.ext_proc.v3.ExternalProcessor",
                                    },
                                    "initialDelaySeconds": 1,
                                    "periodSeconds": 2,
                                },
                                "volumeMounts": [
                                    {
                                        "name": "config",
                                        "mountPath": "/etc/praxis",
                                        "readOnly": True,
                                    },
                                    {
                                        "name": "credentials",
                                        "mountPath": "/etc/praxis/credentials",
                                        "readOnly": True,
                                    },
                                ],
                            }
                        ],
                        "volumes": [
                            {"name": "config", **source},
                            {
                                "name": "credentials",
                                "projected": {
                                    "sources": [
                                        {
                                            "secret": {
                                                "name": f"credential-{key}",
                                                "items": [
                                                    {
                                                        "key": "api-key",
                                                        "path": f"credential-{key}/api-key",
                                                    }
                                                ],
                                            }
                                        }
                                    ]
                                },
                            },
                        ],
                    },
                }
            )

    kubectl("create", "namespace", namespace)
    try:
        kubectl(
            "apply",
            "-f",
            "-",
            document={"apiVersion": "v1", "kind": "List", "items": objects},
        )
        kubectl(
            "-n",
            namespace,
            "wait",
            "--for=condition=Ready",
            "pod",
            "-l",
            "case=fixed",
            f"--timeout={args.timeout}s",
        )
        for key in ("a", "b"):
            pod_name = f"baseline-{key}"
            kubectl(
                "-n",
                namespace,
                "wait",
                "--for=jsonpath={.status.phase}=Running",
                f"pod/{pod_name}",
                f"--timeout={args.timeout}s",
            )
            deadline = time.monotonic() + args.timeout
            while True:
                logs = kubectl("-n", namespace, "logs", pod_name)
                if "credential_inject: cannot read file" in logs:
                    break
                if time.monotonic() >= deadline:
                    raise AssertionError(
                        f"{pod_name} did not reproduce the missing-credential failure"
                    )
                time.sleep(1)
            pod = json.loads(
                kubectl("-n", namespace, "get", "pod", pod_name, "-o", "json")
            )
            assert not any(
                c["type"] == "Ready" and c["status"] == "True"
                for c in pod["status"]["conditions"]
            )
            actual = kubectl(
                "-n",
                namespace,
                "exec",
                f"fixed-{key}",
                "--",
                "sha256sum",
                "/etc/praxis/extproc.yaml",
            ).split()[0]
            assert actual == hashlib.sha256(config(key).encode()).hexdigest()
            print(
                f"PASS: {key}: skewed ConfigMap fails startup; pod snapshot serves with matching config"
            )

        token = "synthetic-rotated-token-a"
        kubectl("apply", "-f", "-", document=secret("a", token))
        expected = hashlib.sha256(token.encode()).hexdigest()
        deadline = time.monotonic() + args.timeout
        while True:
            actual = kubectl(
                "-n",
                namespace,
                "exec",
                "fixed-a",
                "--",
                "sha256sum",
                "/etc/praxis/credentials/credential-a/api-key",
            ).split()[0]
            if actual == expected:
                break
            if time.monotonic() >= deadline:
                raise AssertionError(
                    "projected Secret rotation did not reach the fixed pod"
                )
            time.sleep(2)
        kubectl(
            "-n",
            namespace,
            "wait",
            "--for=condition=Ready",
            "pod",
            "-l",
            "case=fixed",
            f"--timeout={args.timeout}s",
        )
        print(
            "PASS: Secret projection still rotates while startup config remains pod-bound"
        )
    finally:
        kubectl("delete", "namespace", namespace, "--wait=false")


if __name__ == "__main__":
    main()
