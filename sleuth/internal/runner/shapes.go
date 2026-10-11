package runner

import (
	"sleuth/internal/report"
	"sleuth/internal/shape"
)

// stepShape is the structure of what a step returned, or nil if it returned nothing a shape
// can describe. Only JSON responses count: a body that was not JSON is recorded as text and
// has no structure. A database step is described by its rows.
func stepShape(s *report.Step) *shape.Entry {
	switch {
	case s.Response != nil:
		if s.Response.Body == nil {
			return nil
		}
		if _, isText := s.Response.Body.(string); isText { // not JSON (or a bare JSON string)
			return nil
		}
		e := &shape.Entry{Of: "response", Status: s.Response.Status, Observations: 1, Shape: shape.Infer(s.Response.Body)}
		if s.Contract != nil {
			e.Operation = s.Contract.Operation
		}
		return e
	case s.DB != nil && s.Status == report.Passed:
		rows := make([]any, 0, len(s.DB.Rows))
		for _, r := range s.DB.Rows {
			rows = append(rows, r)
		}
		return &shape.Entry{Of: "rows", Observations: 1, Shape: shape.Infer(rows)}
	}
	return nil
}
