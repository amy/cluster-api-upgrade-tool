// Copyright 2019 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
)

func TestEtcdMemberHealthStructDecoding(t *testing.T) {
	data := `{
		"header": {
			"cluster_id":14841639068965178418,
			"member_id":10276657743932975437,
			"raft_term":444
		},
		"members": [
			{
				"ID":5782640540428238474,
				"name":"two",
				"peerURLs":["http://localhost:3380"],
				"clientURLs":["http://localhost:3379"]
			},
			{
				"ID":10276657743932975437,
				"name":"default",
				"peerURLs":["http://localhost:2380"],
				"clientURLs":["http://localhost:2379"]
			}
		]
	}`

	var r etcdMembersResponse

	if err := json.Unmarshal([]byte(data), &r); err != nil {
		t.Fatalf("%+v", err)
	}

	expected := etcdMembersResponse{
		Members: []etcdMember{
			{Name: "two", ID: 5782640540428238474, ClientURLs: []string{"http://localhost:3379"}},
			{Name: "default", ID: 10276657743932975437, ClientURLs: []string{"http://localhost:2379"}},
		},
	}

	if !reflect.DeepEqual(expected, r) {
		t.Errorf("expected %#v, got %#v", expected, r)
	}
}

func TestUpdateKubeadmKubernetesVersion(t *testing.T) {
	generate := func(version string) string {
		return fmt.Sprintf(`apiVersion: v1
data:
  ClusterConfiguration: |
    apiServer:
      certSANs:
      - 10.0.0.227
      - example.com
      extraArgs:
        authorization-mode: Node,RBAC
        cloud-provider: aws
      timeoutForControlPlane: 4m0s
    apiVersion: kubeadm.k8s.io/v1beta1
    certificatesDir: /etc/kubernetes/pki
    clusterName: test1
    controlPlaneEndpoint: example.com:6443
    controllerManager:
      extraArgs:
        cloud-provider: aws
    dns:
      type: CoreDNS
    etcd:
      local:
        dataDir: /var/lib/etcd
    imageRepository: k8s.gcr.io
    kind: ClusterConfiguration
    kubernetesVersion: %s
    networking:
      dnsDomain: cluster.local
      podSubnet: 192.168.0.0/16
      serviceSubnet: 10.96.0.0/12
    scheduler: {}
  ClusterStatus: |
    apiEndpoints:
      ip-10-0-0-197.ec2.internal:
        advertiseAddress: 10.0.0.197
        bindPort: 6443
      ip-10-0-0-227.ec2.internal:
        advertiseAddress: 10.0.0.227
        bindPort: 6443
    apiVersion: kubeadm.k8s.io/v1beta1
    kind: ClusterStatus
kind: ConfigMap
metadata:
  creationTimestamp: "2019-07-03T18:17:01Z"
  name: kubeadm-config
  namespace: kube-system
  resourceVersion: "1312"
  selfLink: /api/v1/namespaces/kube-system/configmaps/kubeadm-config
  uid: c0d8ace7-9dbe-11e9-bfe7-129245863a50
`, version)
	}

	originalYaml := generate("v1.13.7")

	updatedVersion := "v1.14.3"
	expectedYaml := generate(updatedVersion)

	original := new(v1.ConfigMap)
	_, _, err := scheme.Codecs.UniversalDecoder(v1.SchemeGroupVersion).Decode([]byte(originalYaml), nil, original)
	if err != nil {
		t.Fatal(err)
	}

	updatedCM, err := updateKubeadmKubernetesVersion(original, updatedVersion)
	if err != nil {
		t.Fatal(err)
	}

	updatedYaml, err := yaml.Marshal(updatedCM)
	if err != nil {
		t.Fatal(err)
	}

	if strings.TrimSpace(expectedYaml) != strings.TrimSpace(string(updatedYaml)) {
		t.Errorf("expected %s, got %s", expectedYaml, updatedYaml)
	}
}
