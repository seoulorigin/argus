//go:build !linux

package agent

import "argus/internal/entities/container"

type podCollector struct{}

func newPodCollector() *podCollector { return nil }

func (c *podCollector) Collect(cacheTimeMs uint16) []*container.Stats { return nil }

func (c *podCollector) NetnsPath(uid string) (string, bool) { return "", false }
