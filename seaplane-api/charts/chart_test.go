package charts

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"gopkg.in/yaml.v3"
)

type manifest struct {
	Kind     string
	Metadata struct{ Name string }
	Data     map[string]string
	Spec     struct {
		Template struct {
			Metadata struct{ Annotations map[string]string }
			Spec     struct {
				Containers []struct {
					Env          []struct{ Name, Value string }
					VolumeMounts []struct {
						Name      string
						MountPath string `yaml:"mountPath"`
						ReadOnly  bool   `yaml:"readOnly"`
					} `yaml:"volumeMounts"`
				}
				Volumes []struct {
					Name      string
					ConfigMap struct{ Name string } `yaml:"configMap"`
				}
			}
		}
	}
}

func helmTemplate(t *testing.T, overrides ...string) ([]byte, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	args := []string{"template", "platform-api", "."}
	for _, value := range overrides {
		args = append(args, "--set-string", value)
	}
	return exec.Command(helm, args...).CombinedOutput()
}

func renderedManifests(t *testing.T, overrides ...string) map[string]manifest {
	t.Helper()
	content, err := helmTemplate(t, overrides...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, content)
	}
	docs := map[string]manifest{}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var doc manifest
		if err := decoder.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		docs[doc.Kind+"/"+doc.Metadata.Name] = doc
	}
	return docs
}

func TestInstallerRoleChart(t *testing.T) {

	settings := []string{
		"global.aws_region=us-east-1",
		"platformApi.installerRole.environmentId=ephemeral-example-123",
		"platformApi.installerRole.jumpRoleArn=arn:aws:iam::123456789012:role/example-jump",
		"platformApi.installerRole.accountAccess.tableArn=arn:aws:dynamodb:us-west-2:210987654321:table/example-account-access",
		"platformApi.installerRole.accountAccess.tableRegion=us-west-2",
	}
	enabled := renderedManifests(t, settings...)
	cm, exists := enabled["ConfigMap/installer-role-config"]
	if !exists {
		t.Fatal("installer-role ConfigMap missing")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(cm.Data["config.yaml"]), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadInstallerRoleConfig(path)
	if err != nil || cfg.EnvironmentID != "ephemeral-example-123" || cfg.AccountAccess.TableRegion != "us-west-2" {
		t.Fatalf("rendered file is not a valid API configuration: %+v, %v", cfg, err)
	}
	deployment := enabled["Deployment/platform-api"]
	pod := deployment.Spec.Template.Spec
	env := map[string]string{}
	for _, entry := range pod.Containers[0].Env {
		env[entry.Name] = entry.Value
	}
	if env["INSTALLER_ROLE_CONFIG_FILE"] != "/etc/platform-api/installer-role/config.yaml" || env["AWS_REGION"] != "us-east-1" {
		t.Fatalf("wrong deployment env: %+v", env)
	}
	mounted, volume := false, false
	for _, mount := range pod.Containers[0].VolumeMounts {
		if mount.Name == "installer-role-config" {
			mounted = mount.ReadOnly && mount.MountPath == filepath.Dir(env["INSTALLER_ROLE_CONFIG_FILE"])
		}
	}
	for _, v := range pod.Volumes {
		if v.Name == "installer-role-config" {
			volume = v.ConfigMap.Name == cm.Metadata.Name
		}
	}
	if !mounted || !volume {
		t.Fatal("ConfigMap must be mounted read-only at the configured file's directory")
	}
	checksum := deployment.Spec.Template.Metadata.Annotations["checksum/installer-role-config"]
	if len(checksum) != 64 {
		t.Fatal("missing pod configuration checksum")
	}
	for _, change := range []string{
		"environmentId=ephemeral-second",
		"jumpRoleArn=arn:aws:iam::123456789012:role/second-jump",
		"installerManagedPolicyArn=changed",
		"trustTemplateVersion=2",
		"formatVersion=2",
		"accountAccess.tableArn=arn:aws:dynamodb:us-west-2:210987654321:table/second-table",
		"accountAccess.tableRegion=eu-west-1",
	} {
		t.Run("roll on "+strings.Split(change, "=")[0], func(t *testing.T) {
			changed := renderedManifests(t, append(append([]string{}, settings...), "platformApi.installerRole."+change)...)
			if changed["Deployment/platform-api"].Spec.Template.Metadata.Annotations["checksum/installer-role-config"] == checksum {
				t.Fatal("configuration change did not roll the pod template")
			}
		})
	}
	for _, field := range []string{"environmentId", "jumpRoleArn", "accountAccess.tableArn", "accountAccess.tableRegion"} {
		t.Run("required "+field, func(t *testing.T) {
			out, err := helmTemplate(t, append(append([]string{}, settings...), "platformApi.installerRole."+field+"=")...)
			if err == nil || !strings.Contains(string(out), field+" is required") {
				t.Fatalf("missing value accepted: %v\n%s", err, out)
			}
		})
	}
}
