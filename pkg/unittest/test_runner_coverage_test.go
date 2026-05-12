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
	// Branch coverage totals should be present
	assert.Contains(t, report, "totalBranches")
	assert.Contains(t, report, "coveredBranchEstimate")
	assert.Contains(t, report, "branchCoveragePercent")
}

// TestV3RunnerBranchCoverage verifies that the tracker correctly distinguishes between
// test cases that render a template with different values (exercising different conditional
// branches) and reports a branch-coverage estimate in the console output and JSON report.
func TestV3RunnerBranchCoverage(t *testing.T) {
	chartYaml := `
apiVersion: v2
name: branchcov
version: 0.1.0
`
	// Template with a top-level conditional: 2 branches (if true, if false/empty)
	serviceTemplate := `{{- if .Values.service.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-service
{{- end }}
`
	// Test 1: service enabled (true branch)
	testEnabled := `
suite: service enabled suite
templates:
  - templates/service.yaml
tests:
  - it: should render service when enabled
    set:
      service.enabled: true
    asserts:
      - isKind:
          of: Service
`
	// Test 2: service disabled (false branch – template renders empty)
	testDisabled := `
suite: service disabled suite
templates:
  - templates/service.yaml
tests:
  - it: should not render service when disabled
    set:
      service.enabled: false
    asserts:
      - hasDocuments:
          count: 0
`

	tmp := t.TempDir()
	coverageOutputPath := filepath.Join(tmp, "branch-coverage.json")

	files := map[string][]byte{
		"chart/Chart.yaml":                       []byte(chartYaml),
		"chart/templates/service.yaml":            []byte(serviceTemplate),
		"chart/tests/enabled_test.yaml":           []byte(testEnabled),
		"chart/tests/disabled_test.yaml":          []byte(testDisabled),
	}
	for relPath, data := range files {
		fullPath := filepath.Join(tmp, relPath)
		assert.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
		assert.NoError(t, os.WriteFile(fullPath, data, 0644))
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

	// Template-level: 1 of 1 covered
	assert.Contains(t, buffer.String(), "Template Coverage: 1 of 1 templates covered (100.0%)")
	// Branch summary line should appear
	assert.Contains(t, buffer.String(), "Branch Coverage (est.):")

	reportBytes, err := os.ReadFile(coverageOutputPath)
	assert.NoError(t, err)

	var report map[string]any
	assert.NoError(t, json.Unmarshal(reportBytes, &report))

	assert.EqualValues(t, 1, report["totalTemplates"])
	assert.EqualValues(t, 1, report["coveredTemplates"])

	// The template has 1 {{if}} → 2 branch points. Two distinct renders (non-empty + empty)
	// should drive the estimate to 2/2 = 100%.
	assert.EqualValues(t, 2, report["totalBranches"])
	assert.EqualValues(t, 2, report["coveredBranchEstimate"])
	assert.InDelta(t, 100.0, report["branchCoveragePercent"], 0.01)

	// Per-file details
	files2 := report["files"].([]any)
	assert.Len(t, files2, 1)
	fileEntry := files2[0].(map[string]any)
	assert.Equal(t, "branchcov/templates/service.yaml", fileEntry["template"])
	assert.InDelta(t, 100.0, fileEntry["branchCoveragePercent"], 0.01)
}

// TestV3RunnerBranchCoveragePartial verifies that when only one branch of a conditional
// is exercised (e.g., service always enabled), the branch coverage estimate is less than 100%.
func TestV3RunnerBranchCoveragePartial(t *testing.T) {
	chartYaml := `
apiVersion: v2
name: partial
version: 0.1.0
`
	serviceTemplate := `{{- if .Values.service.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-service
{{- end }}
`
	// Only tests the enabled=true path
	testEnabled := `
suite: service always enabled
templates:
  - templates/service.yaml
tests:
  - it: should render service
    set:
      service.enabled: true
    asserts:
      - isKind:
          of: Service
`

	tmp := t.TempDir()
	coverageOutputPath := filepath.Join(tmp, "partial-coverage.json")

	files := map[string][]byte{
		"chart/Chart.yaml":                  []byte(chartYaml),
		"chart/templates/service.yaml":       []byte(serviceTemplate),
		"chart/tests/service_test.yaml":      []byte(testEnabled),
	}
	for relPath, data := range files {
		fullPath := filepath.Join(tmp, relPath)
		assert.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
		assert.NoError(t, os.WriteFile(fullPath, data, 0644))
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

	reportBytes, err := os.ReadFile(coverageOutputPath)
	assert.NoError(t, err)

	var report map[string]any
	assert.NoError(t, json.Unmarshal(reportBytes, &report))

	// Only the true branch was exercised → estimate = 1 of 2 = 50%
	assert.EqualValues(t, 2, report["totalBranches"])
	assert.EqualValues(t, 1, report["coveredBranchEstimate"])
	assert.InDelta(t, 50.0, report["branchCoveragePercent"], 0.01)

	// The "Low Branch Coverage" section should appear in the console output
	assert.Contains(t, buffer.String(), "Low Branch Coverage:")
}
