package controller

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	ctrlv1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	nodev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	ElementalUpgradeRequestAnnotationKey    = "harvesterhci.io/elemental-upgrade-requested"
	ElementalUpgradeRequestAnnotationValue  = "true"
	ElementalUpgradeStatusCondition         = "ElementalUpgradeStatus"
	ElementalUpgradeArtifacts               = "/tmp/elemental-upgrade-artifacts"
	ElementalUpgradeArtifactsAnnotationKey  = "harvesterhci.io/elemental-upgrade-artifacts"
	elementalBinaryName                     = "elemental"
	defaultElementalPath                    = "/usr/bin"
	configDirectory                         = "config"
	rootfsDirectory                         = "rootfs"
	configFileName                          = "config.yaml"
	rancherdConfigDirectory                 = "/etc/rancher/rancherd"
	ElementalOverrideCommandAnnotationKey   = "harvesterhci.io/elemental-override-command"
	ElementalOverrideArgumentsAnnotationKey = "harvesterhci.io/elemental-override-arguments"
	ElementalUpgradeSuccessfulReason        = "ElementalUpgradeSuccessful"
	ElementalUpgradeFailedReason            = "ElementalUpgradeFailed"
	UpgradeScriptName                       = "elemental-upgrade.sh"
	templateScript                          = `#!/bin/sh
set -ex

STATE_DIR="/run/initramfs/cos-state"
export UPGRADE_ARTIFACTS_DIR="{{.ELEMENTAL_UPGRADE_ARTIFACTS_DIR}}"
export PATH=".:/sbin:/usr/sbin:/usr/local/sbin:/root/bin:/usr/local/bin:/usr/bin:/bin:/var/lib/rancher/rke2/bin"

cleanup_incomplete_state_file() {
  STATE_FILE=${STATE_DIR}/state.yaml

  INCOMPLETE_STATE_FILE=0
  if [[ ! -f $STATE_FILE ]]; then
      INCOMPLETE_STATE_FILE=0
  else
    found=$(yq '.state| has("label")' ${STATE_FILE})
    if [[ $found == "false" ]]; then
        INCOMPLETE_STATE_FILE=1
    fi
  fi

  if [[ $INCOMPLETE_STATE_FILE == 1 ]]; then
    echo "Start to remove the incomplete state.yaml"
    mount -o rw,remount ${STATE_DIR}
    cp ${STATE_FILE} ${STATE_FILE}.bak
    rm -f ${STATE_FILE}
    mount -o ro,remount ${STATE_DIR}
  fi
}
echo "Cleanup state.yaml if incomplete"

cat > $UPGRADE_ARTIFACTS_DIR/config/config.yaml <<EOF
upgrade:
  system:
    size: 3072
EOF

# Allow override of upgrade.yaml from /etc/rancher/rancherd/
if [ -f /etc/rancher/rancherd/upgrade.yaml ]; then
	cp /etc/rancher/rancherd/upgrade.yaml $UPGRADE_ARTIFACTS_DIR/config/config.yaml
fi

cleanup_incomplete_state_file
echo "Starting elemental upgrade..."
elemental upgrade --directory $UPGRADE_ARTIFACTS_DIR/rootfs --config-dir $UPGRADE_ARTIFACTS_DIR/config --debug
echo "Elemental upgrade completed successfully."
`
)

type ElementalUpgradeManager struct {
	ctx            context.Context
	nodeController nodev1.NodeInterface
	nodeName       string
}

func Register(ctx context.Context, nodeController ctrlv1.NodeController, nodeInterface nodev1.NodeInterface, nodeName string) error {
	mgr := &ElementalUpgradeManager{
		ctx:            ctx,
		nodeController: nodeInterface,
		nodeName:       nodeName,
	}
	nodeController.OnChange(ctx, "rancherd-node-controller", mgr.OnChange)
	return nil
}

func (m *ElementalUpgradeManager) OnChange(_ string, nodeObj *corev1.Node) (*corev1.Node, error) {
	if nodeObj == nil || nodeObj.DeletionTimestamp != nil || nodeObj.Annotations[ElementalUpgradeRequestAnnotationKey] != ElementalUpgradeRequestAnnotationValue {
		return nodeObj, nil
	}

	// perform elemental upgrade if needed
	return m.runElementalUpgrade(nodeObj)
}

// runElementalUpgrade executes the elemental upgrade process for the given node using the specified artifacts and configuration.
func (m *ElementalUpgradeManager) runElementalUpgrade(nodeObj *corev1.Node) (*corev1.Node, error) {
	// to ensure upgrade is only run on a node where it was successful, we need to ensure that elemental upgrade condition
	// has not been set on the node
	exists := checkElementalUpgradeConditionExists(nodeObj)
	if exists {
		logrus.Infof("elemental upgrade has already been attempted on the node, skipping node %s", m.nodeName)
		return nodeObj, nil
	}

	artifactPath := ElementalUpgradeArtifacts

	val, ok := nodeObj.Annotations[ElementalUpgradeArtifactsAnnotationKey]
	if ok && val != "" {
		artifactPath = val
	}

	// default upgrade script
	command := filepath.Join("/tmp", UpgradeScriptName)
	args := []string{}

	scriptContent, err := generateElementalUpgradeScript(artifactPath)
	if err != nil {
		logrus.Errorf("failed to generate upgrade script: %v", err)
		return nodeObj, err
	}

	if err := os.WriteFile(command, scriptContent.Bytes(), 0755); err != nil {
		logrus.Errorf("failed to write upgrade script: %v", err)
		return nodeObj, err
	}
	// check for elemental override command and arguments. This makes it easier to customize the upgrade process per node.
	// this can also be used to perform unit tests to verify reconcile of nodes and conditions
	if val, ok = nodeObj.Annotations[ElementalOverrideCommandAnnotationKey]; ok && val != "" {
		command = val
	}

	if val, ok = nodeObj.Annotations[ElementalOverrideArgumentsAnnotationKey]; ok && val != "" {
		args = strings.Fields(val)
	}

	logrus.Debugf("Running elemental upgrade with binary: %s and args: %v\n", command, args)
	cmd := exec.Command(command, args...)

	err = runCommand(cmd)
	if err != nil {
		logrus.Errorf("error during command execution: %v", err)
	}
	return m.updateElementalUpgradeCondition(nodeObj, err)
}

// check if ElementalUpgradeStatusCondition exists and return its value
func checkElementalUpgradeConditionExists(nodeObj *corev1.Node) bool {
	for _, condition := range nodeObj.Status.Conditions {
		if condition.Type == ElementalUpgradeStatusCondition {
			return true
		}
	}
	return false
}

func (m *ElementalUpgradeManager) updateElementalUpgradeCondition(nodeObj *corev1.Node, err error) (*corev1.Node, error) {
	condition := corev1.NodeCondition{
		Type:               ElementalUpgradeStatusCondition,
		LastHeartbeatTime:  metav1.Now(),
		LastTransitionTime: metav1.Now(),
	}
	if err == nil {
		condition.Status = corev1.ConditionTrue
		condition.Reason = ElementalUpgradeSuccessfulReason
		condition.Message = "Elemental upgrade succeeded"
	} else {
		condition.Status = corev1.ConditionFalse
		condition.Reason = ElementalUpgradeFailedReason
		condition.Message = err.Error()
	}
	nodeObj.Status.Conditions = append(nodeObj.Status.Conditions, condition)
	return m.nodeController.UpdateStatus(m.ctx, nodeObj, metav1.UpdateOptions{})
}

func runCommand(cmd *exec.Cmd) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}

	if scanError := scanner.Err(); scanError != nil {
		log.Printf("Scanner error: %v", scanError)
	}

	err = cmd.Wait()
	return err
}

func generateElementalUpgradeScript(artifactPath string) (bytes.Buffer, error) {
	values := make(map[string]interface{})
	values["ELEMENTAL_UPGRADE_ARTIFACTS_DIR"] = artifactPath
	var output bytes.Buffer
	tmpl, err := template.New("upgrade").Funcs(sprig.TxtFuncMap()).Parse(templateScript)
	if err != nil {
		return output, fmt.Errorf("failed to parse template: %v", err)
	}
	err = tmpl.Execute(&output, values)
	if err != nil {
		return output, fmt.Errorf("failed to execute template: %v", err)
	}
	return output, nil
}
