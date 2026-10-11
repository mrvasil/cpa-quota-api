package main

func reconcileActiveWindows(provider string, previous, incoming []window) []window {
	incoming = filterWindows(provider, incoming)
	previous = filterWindows(provider, previous)
	if len(incoming) == 0 {
		return previous
	}
	present := map[string]bool{}
	for _, w := range incoming {
		present[w.Group+"\x00"+w.Key] = true
	}
	retained := []window{}
	for _, w := range previous {
		if present[w.Group+"\x00"+w.Key] {
			retained = append(retained, w)
		}
	}
	return mergeWindows(retained, incoming)
}
