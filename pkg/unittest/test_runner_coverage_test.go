package unittest_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	. "github.com/helm-unittest/helm-unittest/pkg/unittest"
	"github.com/helm-unittest/helm-unittest/pkg/unittest/printer"
	"github.com/stretchr/testify/assert"
)

func TestV3RunnerCoverageSummaryAndJsonReport(t *testing.T) {
	chart := `
apiVersion: v2
name: coverage
version: 0.1.0
`
	deploymentTemplate := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-deployment
`
	serviceTemplate := `
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-service
`
	testSuite := `
suite: coverage suite
templates:
  - templates/deployment.yaml
tests:
  - it: should render deployment
    asserts:
      - isKind:
          of: Deployment
`

	tmp := t.TempDir()
	coverageOutputPath := filepath.Join(tmp, "coverage-report.json")

	fs := fstest.MapFS{
		"chart/Chart.yaml":                 {Data: []byte(chart)},
		"chart/templates/deployment.yaml":  {Data: []byte(deploymentTemplate)},
		"chart/templates/service.yaml":     {Data: []byte(serviceTemplate)},
		"chart/tests/deployment_test.yaml": {Data: []byte(testSuite)},
	}
	for filePath, file := range fs {
		err := os.MkdirAll(filepath.Dir(filepath.Join(tmp, filePath)), 0755)
		assert.NoError(t, err)
		err = os.WriteFile(filepath.Join(tmp, filePath), file.Data, 0644)
		assert.NoError(t, err)
	}

	buffer := new(bytes.Buffer)
	runner := TestRunner{
		Printer:            printer.NewPrinter(buffer, nil),
		TestFiles:          []string{"tests/*_test.yaml"},
		Coverage:           true,
		CoverageOutputFile: coverageOutputPath,
	}

	passed := runner.RunV3([]string{filepath.Join(tmp, "chart")})
	assert.True(t, passed, buffer.String())
	assert.Contains(t, buffer.String(), "Template Coverage: 1 of 2 templates covered (50.0%)")
	assert.Contains(t, buffer.String(), "- coverage/templates/service.yaml")

	reportBytes, err := os.ReadFile(coverageOutputPath)
	assert.NoError(t, err)

	report := map[string]any{}
	err = json.Unmarshal(reportBytes, &report)
	assert.NoError(t, err)
	assert.EqualValues(t, 2, report["totalTemplates"])
	assert.EqualValues(t, 1, report["coveredTemplates"])
}
