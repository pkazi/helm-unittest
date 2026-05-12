package unittest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v3chart "helm.sh/helm/v3/pkg/chart"
)

type templateCoverageFile struct {
	Template string `json:"template"`
	Hits     uint   `json:"hits"`
	Covered  bool   `json:"covered"`
}

type templateCoverageReport struct {
	TotalTemplates   int                    `json:"totalTemplates"`
	CoveredTemplates int                    `json:"coveredTemplates"`
	CoveragePercent  float64                `json:"coveragePercent"`
	Files            []templateCoverageFile `json:"files"`
}

type templateCoverageTracker struct {
	expectedTemplates map[string]struct{}
	templateHits      map[string]uint
}

func newTemplateCoverageTracker() *templateCoverageTracker {
	return &templateCoverageTracker{
		expectedTemplates: make(map[string]struct{}),
		templateHits:      make(map[string]uint),
	}
}

func (tracker *templateCoverageTracker) addChart(chartRoute string, chart *v3chart.Chart, includeSubcharts bool) {
	for _, template := range chart.Templates {
		if strings.HasPrefix(filepath.Base(template.Name), "_") {
			continue
		}
		tracker.expectedTemplates[filepath.ToSlash(filepath.Join(chartRoute, template.Name))] = struct{}{}
	}

	if !includeSubcharts {
		return
	}

	for _, dependency := range chart.Dependencies() {
		dependencyRoute := filepath.Join(chartRoute, subchartPrefix, dependency.Name())
		tracker.addChart(dependencyRoute, dependency, true)
	}
}

func (tracker *templateCoverageTracker) addRenderedTemplates(renderedTemplates []string) {
	for _, renderedTemplate := range renderedTemplates {
		if _, exists := tracker.expectedTemplates[renderedTemplate]; exists {
			tracker.templateHits[renderedTemplate]++
		}
	}
}

func (tracker *templateCoverageTracker) report() templateCoverageReport {
	report := templateCoverageReport{}
	if len(tracker.expectedTemplates) == 0 {
		return report
	}

	templateFiles := make([]string, 0, len(tracker.expectedTemplates))
	for templateFile := range tracker.expectedTemplates {
		templateFiles = append(templateFiles, templateFile)
	}
	sort.Strings(templateFiles)

	report.Files = make([]templateCoverageFile, 0, len(templateFiles))
	for _, templateFile := range templateFiles {
		hits := tracker.templateHits[templateFile]
		covered := hits > 0
		if covered {
			report.CoveredTemplates++
		}
		report.Files = append(report.Files, templateCoverageFile{
			Template: templateFile,
			Hits:     hits,
			Covered:  covered,
		})
	}

	report.TotalTemplates = len(report.Files)
	report.CoveragePercent = float64(report.CoveredTemplates) * 100 / float64(report.TotalTemplates)

	return report
}

func (tracker *templateCoverageTracker) write(reportPath string) error {
	reportFile, fileErr := os.Create(reportPath)
	if fileErr != nil {
		return fileErr
	}
	defer func() {
		_ = reportFile.Close()
	}()

	reportEncoder := json.NewEncoder(reportFile)
	reportEncoder.SetIndent("", "  ")
	if encodeErr := reportEncoder.Encode(tracker.report()); encodeErr != nil {
		return fmt.Errorf("failed to write coverage report: %w", encodeErr)
	}

	return nil
}
