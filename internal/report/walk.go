package report

// Flatten lists every step of a run, subflow steps included, each with an id that is its path
// from the top flow ("auth/login"). The copies have no Children. Consumers that look at one
// step at a time (drift, coverage, shapes, exit codes) read this instead of the nested form.
func Flatten(steps []Step) []Step {
	var out []Step
	var walk func(prefix string, ss []Step)
	walk = func(prefix string, ss []Step) {
		for _, s := range ss {
			kids := s.Children
			s.ID = prefix + s.ID
			s.Children = nil
			out = append(out, s)
			walk(s.ID+"/", kids)
		}
	}
	walk("", steps)
	return out
}

// Leaves lists the steps that carry a result of their own, with path ids: steps with no
// children, and a subflow step that failed although none of its children did (for example its
// outputs could not be evaluated). A subflow step that failed because a child failed is left
// out, so one failure is counted once.
func Leaves(steps []Step) []Step {
	var out []Step
	var walk func(prefix string, ss []Step)
	walk = func(prefix string, ss []Step) {
		for _, s := range ss {
			kids := s.Children
			s.ID = prefix + s.ID
			s.Children = nil
			if len(kids) == 0 || (s.Status == Failed && !anyFailed(kids)) {
				out = append(out, s)
			}
			walk(s.ID+"/", kids)
		}
	}
	walk("", steps)
	return out
}

func anyFailed(ss []Step) bool {
	for _, s := range ss {
		if s.Status == Failed {
			return true
		}
	}
	return false
}
