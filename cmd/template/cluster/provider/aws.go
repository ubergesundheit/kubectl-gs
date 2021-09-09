package provider

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"text/template"

	"github.com/giantswarm/apiextensions/v3/pkg/annotation"
	"github.com/giantswarm/apiextensions/v3/pkg/apis/infrastructure/v1alpha3"
	"github.com/giantswarm/microerror"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	capav1alpha3 "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha3"
	capiv1alpha3 "sigs.k8s.io/cluster-api/api/v1alpha3"
	bootstrap "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	kubeadm "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1alpha3"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/kubectl-gs/internal/key"
	"github.com/giantswarm/kubectl-gs/internal/label"
)

func WriteAWSTemplate(out io.Writer, config ClusterCRsConfig) error {
	var err error

	if key.IsCAPAVersion(config.ReleaseVersion) {
		err = WriteCAPATemplate(out, config)
		if err != nil {
			return microerror.Mask(err)
		}
	} else {
		err = WriteGSAWSTemplate(out, config)
		if err != nil {
			return microerror.Mask(err)
		}
	}

	return nil
}

func WriteCAPATemplate(out io.Writer, config ClusterCRsConfig) error {
	var err error
	var awsRegion string

	if config.ExternalSNAT {
		return microerror.Maskf(invalidFlagError, "--external-snat setting is not available for release %v", config.ReleaseVersion)
	}
	if config.PodsCIDR != "" {
		return microerror.Maskf(invalidFlagError, "--pods-cidr setting is not available for release %v", config.ReleaseVersion)
	}

	clusterTemplate, err := getCAPAClusterTemplate(config)
	if err != nil {
		return err
	}

	var sshSSOPublicKey string
	{
		sshSSOPublicKey, err = key.SSHSSOPublicKey()
		if err != nil {
			return microerror.Mask(err)
		}
	}

	data := struct {
		AWSClusterCR             string
		AWSMachineTemplateCR     string
		ClusterCR                string
		KubeadmControlPlaneCR    string
		AWSClusterRoleIdentityCR string

		BastionBootstrapSecret      string
		BastionMachineDeploymentCR  string
		BastionAWSMachineTemplateCR string
	}{}

	crLabels := map[string]string{
		label.ReleaseVersion:            config.ReleaseVersion,
		label.Cluster:                   config.Name,
		capiv1alpha3.ClusterLabelName:   config.Name,
		label.Organization:              config.Owner,
		"cluster.x-k8s.io/watch-filter": "capi"}

	objects := clusterTemplate.Objs()
	for _, o := range objects {
		switch o.GetKind() {
		case "AWSCluster":
			o.SetLabels(crLabels)
			awscluster, err := newAWSClusterFromUnstructured(config, o)
			if err != nil {
				return microerror.Mask(err)
			}
			awsRegion = awscluster.Spec.Region
			awsClusterCRYaml, err := yaml.Marshal(awscluster)
			if err != nil {
				return microerror.Mask(err)
			}
			data.AWSClusterCR = string(awsClusterCRYaml)
		case "AWSMachineTemplate":
			o.SetLabels(crLabels)
			awsmachinetemplate, err := newAWSMachineTemplateFromUnstructured(config, o)
			if err != nil {
				return microerror.Mask(err)
			}
			awsMachineTemplateCRYaml, err := yaml.Marshal(awsmachinetemplate)
			if err != nil {
				return microerror.Mask(err)
			}
			data.AWSMachineTemplateCR = string(awsMachineTemplateCRYaml)
		case "Cluster":
			clusterLabels := crLabels
			for key, value := range config.Labels {
				clusterLabels[key] = value
			}
			o.SetLabels(clusterLabels)
			o.SetAnnotations(map[string]string{annotation.ClusterDescription: config.Description})
			clusterCRYaml, err := yaml.Marshal(o.Object)
			if err != nil {
				return microerror.Mask(err)
			}
			data.ClusterCR = string(clusterCRYaml)
		case "KubeadmControlPlane":
			o.SetLabels(crLabels)
			kubeadmControlPlane, err := newKubeadmControlPlaneFromUnstructured(sshSSOPublicKey, key.NodeSSHDConfigEncoded(), o)
			if err != nil {
				return microerror.Mask(err)
			}
			kubeadmControlPlaneCRYaml, err := yaml.Marshal(kubeadmControlPlane)
			if err != nil {
				return microerror.Mask(err)
			}

			data.KubeadmControlPlaneCR = string(kubeadmControlPlaneCRYaml)
		}
	}
	{
		awsclusterroleidentity := newAWSClusterRoleIdentity(config)
		awsclusterroleidentity.SetLabels(crLabels)
		awsClusterRoleIdentityCRYaml, err := yaml.Marshal(awsclusterroleidentity)
		if err != nil {
			return microerror.Mask(err)
		}
		data.AWSClusterRoleIdentityCR = string(awsClusterRoleIdentityCRYaml)
	}
	// prepare CRs and resources for bastion
	{
		bastionSecret, err := newBastionBootstrapSecret(config, key.BastionSSHDConfigEncoded(), sshSSOPublicKey)
		if err != nil {
			return microerror.Mask(err)
		}
		bastionSecret.SetLabels(crLabels)
		bastionSecret.Labels[key.CAPIRoleLabel] = key.RoleBastion
		bastionSecretYaml, err := yaml.Marshal(bastionSecret)
		if err != nil {
			return microerror.Mask(err)
		}
		data.BastionBootstrapSecret = string(bastionSecretYaml)

		md := newBastionMachineDeployment(config)
		md.SetLabels(crLabels)
		md.Labels[key.CAPIRoleLabel] = key.RoleBastion
		mdYaml, err := yaml.Marshal(md)
		if err != nil {
			return microerror.Mask(err)
		}
		data.BastionMachineDeploymentCR = string(mdYaml)

		awsmachinetemplate := newBastionAWSMachineTemplate(config, awsRegion)
		awsmachinetemplate.SetLabels(crLabels)
		awsmachinetemplate.Labels[key.CAPIRoleLabel] = key.RoleBastion
		awsmachinetemplateYaml, err := yaml.Marshal(awsmachinetemplate)
		if err != nil {
			return microerror.Mask(err)
		}
		data.BastionAWSMachineTemplateCR = string(awsmachinetemplateYaml)
	}

	t := template.Must(template.New(config.FileName).Parse(key.ClusterCAPACRsTemplate))
	err = t.Execute(out, data)
	if err != nil {
		return microerror.Mask(err)
	}

	return nil
}

func WriteGSAWSTemplate(out io.Writer, config ClusterCRsConfig) error {
	var err error

	crsConfig := v1alpha3.ClusterCRsConfig{
		ClusterID: config.Name,

		ExternalSNAT:   config.ExternalSNAT,
		MasterAZ:       config.ControlPlaneAZ,
		Description:    config.Description,
		PodsCIDR:       config.PodsCIDR,
		Owner:          config.Owner,
		ReleaseVersion: config.ReleaseVersion,
		Labels:         config.Labels,
	}

	crs, err := v1alpha3.NewClusterCRs(crsConfig)
	if err != nil {
		return microerror.Mask(err)
	}

	if config.ControlPlaneSubnet != "" {
		crs.AWSCluster.Annotations[annotation.AWSSubnetSize] = config.ControlPlaneSubnet
	}

	if key.IsOrgNamespaceVersion(config.ReleaseVersion) {
		crs = moveCRsToOrgNamespace(crs, config.Owner)
	}

	clusterCRYaml, err := yaml.Marshal(crs.Cluster)
	if err != nil {
		return microerror.Mask(err)
	}

	awsClusterCRYaml, err := yaml.Marshal(crs.AWSCluster)
	if err != nil {
		return microerror.Mask(err)
	}

	g8sControlPlaneCRYaml, err := yaml.Marshal(crs.G8sControlPlane)
	if err != nil {
		return microerror.Mask(err)
	}

	awsControlPlaneCRYaml, err := yaml.Marshal(crs.AWSControlPlane)
	if err != nil {
		return microerror.Mask(err)
	}

	data := struct {
		AWSClusterCR      string
		AWSControlPlaneCR string
		ClusterCR         string
		G8sControlPlaneCR string
	}{
		AWSClusterCR:      string(awsClusterCRYaml),
		ClusterCR:         string(clusterCRYaml),
		G8sControlPlaneCR: string(g8sControlPlaneCRYaml),
		AWSControlPlaneCR: string(awsControlPlaneCRYaml),
	}

	t := template.Must(template.New(config.FileName).Parse(key.ClusterAWSCRsTemplate))
	err = t.Execute(out, data)
	if err != nil {
		return microerror.Mask(err)
	}

	return nil
}

func getCAPAClusterTemplate(config ClusterCRsConfig) (client.Template, error) {
	var err error

	c, err := client.New("")
	if err != nil {
		return nil, err
	}

	templateOptions := client.GetClusterTemplateOptions{
		ClusterName:       config.Name,
		TargetNamespace:   key.OrganizationNamespaceFromName(config.Owner),
		KubernetesVersion: "v1.19.9",
		ProviderRepositorySource: &client.ProviderRepositorySourceOptions{
			InfrastructureProvider: "aws:v0.6.8",
			Flavor:                 "machinepool",
		},
	}
	// Set all environment variables expected by the upstream client to empty strings. These values are defaulted later
	// Make sure that the values are reset.
	for _, envVar := range key.GetCAPAEnvVars() {
		if os.Getenv(envVar) != "" {
			prevEnv := os.Getenv(envVar)
			os.Setenv(envVar, "")
			defer os.Setenv(envVar, prevEnv)
		} else {
			os.Setenv(envVar, "")
			defer os.Unsetenv(envVar)
		}
	}

	if replicas := int64(len(config.ControlPlaneAZ)); replicas > 0 {
		templateOptions.ControlPlaneMachineCount = &replicas
	}

	clusterTemplate, err := c.GetClusterTemplate(templateOptions)
	if err != nil {
		return nil, err
	}
	return clusterTemplate, nil
}

func newAWSClusterFromUnstructured(config ClusterCRsConfig, o unstructured.Unstructured) (*capav1alpha3.AWSCluster, error) {
	var awscluster capav1alpha3.AWSCluster
	{
		err := runtime.DefaultUnstructuredConverter.
			FromUnstructured(o.Object, &awscluster)
		if err != nil {
			return nil, microerror.Mask(err)
		}
		awscluster.Spec.IdentityRef = &capav1alpha3.AWSIdentityReference{
			Name: config.Name,
			Kind: capav1alpha3.ClusterRoleIdentityKind}

		for _, az := range config.ControlPlaneAZ {
			privateSubnet := capav1alpha3.SubnetSpec{AvailabilityZone: az, IsPublic: false}
			publicSubnet := capav1alpha3.SubnetSpec{AvailabilityZone: az, IsPublic: true}
			awscluster.Spec.NetworkSpec.Subnets = append(awscluster.Spec.NetworkSpec.Subnets, &privateSubnet, &publicSubnet)
		}
		if config.ControlPlaneSubnet != "" {
			awscluster.SetAnnotations(map[string]string{annotation.AWSSubnetSize: config.ControlPlaneSubnet})
		}
	}
	return &awscluster, nil
}

func newAWSMachineTemplateFromUnstructured(config ClusterCRsConfig, o unstructured.Unstructured) (*capav1alpha3.AWSMachineTemplate, error) {
	var awsmachinetemplate capav1alpha3.AWSMachineTemplate
	{
		err := runtime.DefaultUnstructuredConverter.
			FromUnstructured(o.Object, &awsmachinetemplate)
		if err != nil {
			return nil, microerror.Mask(err)
		}
		awsmachinetemplate.Spec.Template.Spec.IAMInstanceProfile = key.GetControlPlaneInstanceProfile(config.Name)
		// we need to label so capa-iam-controller knows this is  control-plane awsmachinetemplate and it has to create IAM for it
		awsmachinetemplate.Labels["cluster.x-k8s.io/role"] = "control-plane"
	}
	return &awsmachinetemplate, nil
}

func newKubeadmControlPlaneFromUnstructured(sshSSOPubKey string, sshdConfig string, o unstructured.Unstructured) (*kubeadm.KubeadmControlPlane, error) {
	var cp kubeadm.KubeadmControlPlane
	{
		groups := "sudo"
		shell := "/bin/bash"

		err := runtime.DefaultUnstructuredConverter.
			FromUnstructured(o.Object, &cp)
		if err != nil {
			return nil, microerror.Mask(err)
		}
		cp.Spec.KubeadmConfigSpec.Files = []bootstrap.File{
			{
				Owner:       "root",
				Permissions: "640",
				Path:        "/etc/ssh/sshd_config",
				Content:     sshdConfig,
				Encoding:    bootstrap.Base64,
			},
			{
				Owner:       "root",
				Permissions: "600",
				Path:        "/etc/ssh/trusted-user-ca-keys.pem",
				Content:     sshSSOPubKey,
				Encoding:    bootstrap.Base64,
			},
			{
				Owner:       "root",
				Permissions: "600",
				Path:        "/etc/sudoers.d/giantswarm",
				Content:     key.UbuntuSudoersConfigEncoded(),
				Encoding:    bootstrap.Base64,
			},
		}
		cp.Spec.KubeadmConfigSpec.PostKubeadmCommands = []string{
			"service ssh restart",
		}

		cp.Spec.KubeadmConfigSpec.Users = []bootstrap.User{
			{
				Name:   "giantswarm",
				Groups: &groups,
				Shell:  &shell,
			},
		}
	}

	return &cp, nil
}

func newAWSClusterRoleIdentity(config ClusterCRsConfig) *capav1alpha3.AWSClusterRoleIdentity {
	awsclusterroleidentity := &capav1alpha3.AWSClusterRoleIdentity{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AWSClusterRoleIdentity",
			APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha3",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.Name,
			Namespace: key.OrganizationNamespaceFromName(config.Owner),
		},
		Spec: capav1alpha3.AWSClusterRoleIdentitySpec{
			SourceIdentityRef: &capav1alpha3.AWSIdentityReference{},
			AWSClusterIdentitySpec: capav1alpha3.AWSClusterIdentitySpec{
				AllowedNamespaces: &capav1alpha3.AllowedNamespaces{
					NamespaceList: []string{key.OrganizationNamespaceFromName(config.Owner)},
				},
			},
		},
	}
	return awsclusterroleidentity
}

func newBastionBootstrapSecret(config ClusterCRsConfig, sshdConfig string, sshSSOPublicKey string) (*v1.Secret, error) {
	var ignition string
	{
		data := struct {
			IgnitionFiles []struct {
				Path    string
				Content string
			}
		}{
			IgnitionFiles: []struct {
				Path    string
				Content string
			}{},
		}

		var tpl bytes.Buffer
		t := template.Must(template.New(config.FileName).Parse(fmt.Sprintf(key.BastionIgnitionTemplate, config.Name, sshdConfig, sshSSOPublicKey)))
		err := t.Execute(&tpl, data)
		if err != nil {
			return nil, microerror.Mask(err)
		}

		ignition = base64.StdEncoding.EncodeToString(tpl.Bytes())
	}

	s := &v1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: v1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.BastionResourceName(config.Name),
			Namespace: key.OrganizationNamespaceFromName(config.Owner),
		},
		Type: "cluster.x-k8s.io/secret",
		StringData: map[string]string{
			"value": ignition,
		},
	}

	return s, nil
}

func newBastionMachineDeployment(config ClusterCRsConfig) *capiv1alpha3.MachineDeployment {
	replicas := int32(1)
	rollupdateValue := intstr.FromInt(1)
	dataSecretname := key.BastionResourceName(config.Name)
	// we dont need any specific version for bastion machine as it wont run k8s anyway, but it has to be set for AMI lookup otherwise it will fail
	version := "v0.0.0"

	md := &capiv1alpha3.MachineDeployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "MachineDeployment",
			APIVersion: capiv1alpha3.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.BastionResourceName(config.Name),
			Namespace: key.OrganizationNamespaceFromName(config.Owner),
		},
		Spec: capiv1alpha3.MachineDeploymentSpec{
			ClusterName: config.Name,
			Replicas:    &replicas,
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"cluster.x-k8s.io/cluster-name":    config.Name,
					"cluster.x-k8s.io/deployment-name": key.BastionResourceName(config.Name),
				},
			},
			Strategy: &capiv1alpha3.MachineDeploymentStrategy{
				Type: "RollingUpdate",
				RollingUpdate: &capiv1alpha3.MachineRollingUpdateDeployment{
					MaxSurge:       &rollupdateValue,
					MaxUnavailable: &rollupdateValue,
				},
			},
			Template: capiv1alpha3.MachineTemplateSpec{
				ObjectMeta: capiv1alpha3.ObjectMeta{
					Labels: map[string]string{
						"cluster.x-k8s.io/cluster-name":    config.Name,
						"cluster.x-k8s.io/deployment-name": key.BastionResourceName(config.Name),
					},
				},
				Spec: capiv1alpha3.MachineSpec{
					ClusterName: config.Name,
					Bootstrap: capiv1alpha3.Bootstrap{
						DataSecretName: &dataSecretname,
					},
					InfrastructureRef: v1.ObjectReference{
						APIVersion: capav1alpha3.GroupVersion.String(),
						Kind:       "AWSMachineTemplate",
						Name:       key.BastionResourceName(config.Name),
					},
					Version: &version,
				},
			},
		},
	}

	return md
}

func newBastionAWSMachineTemplate(config ClusterCRsConfig, region string) *capav1alpha3.AWSMachineTemplate {
	uncompressedUserData := true
	publicIP := true
	sshKey := ""

	t := &capav1alpha3.AWSMachineTemplate{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AWSMachineTemplate",
			APIVersion: capav1alpha3.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.BastionResourceName(config.Name),
			Namespace: key.OrganizationNamespaceFromName(config.Owner),
		},
		Spec: capav1alpha3.AWSMachineTemplateSpec{
			Template: capav1alpha3.AWSMachineTemplateResource{
				Spec: capav1alpha3.AWSMachineSpec{
					// ignition do not work with encrypted or compressed user-data so we need to turn it off
					CloudInit: capav1alpha3.CloudInit{
						InsecureSkipSecretsManager: true,
					},
					UncompressedUserData: &uncompressedUserData,
					// leaving it empty as we dont need specific instance profile
					IAMInstanceProfile: "",
					PublicIP:           &publicIP,
					InstanceType:       key.AWSBastionInstanceType,
					AdditionalSecurityGroups: []capav1alpha3.AWSResourceReference{
						{
							Filters: []capav1alpha3.Filter{
								{
									Name:   key.CAPARoleTag,
									Values: []string{"bastion"},
								},
								{
									Name:   key.CAPAClusterOwnedTag(config.Name),
									Values: []string{"owned"},
								},
							},
						},
					},
					SSHKeyName: &sshKey,
					Subnet: &capav1alpha3.AWSResourceReference{
						Filters: []capav1alpha3.Filter{
							{
								Name:   key.CAPARoleTag,
								Values: []string{"public"},
							},
						},
					},
					ImageLookupOrg:    key.FlatcarAWSAccountID(region),
					ImageLookupFormat: "Flatcar-stable-*",
				},
			},
		},
	}

	return t
}

func moveCRsToOrgNamespace(crs v1alpha3.ClusterCRs, organization string) v1alpha3.ClusterCRs {
	crs.Cluster.SetNamespace(key.OrganizationNamespaceFromName(organization))
	crs.Cluster.Spec.InfrastructureRef.Namespace = key.OrganizationNamespaceFromName(organization)
	crs.AWSCluster.SetNamespace(key.OrganizationNamespaceFromName(organization))
	crs.G8sControlPlane.SetNamespace(key.OrganizationNamespaceFromName(organization))
	crs.G8sControlPlane.Spec.InfrastructureRef.Namespace = key.OrganizationNamespaceFromName(organization)
	crs.AWSControlPlane.SetNamespace(key.OrganizationNamespaceFromName(organization))
	return crs
}
