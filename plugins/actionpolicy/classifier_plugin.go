package actionpolicy

import (
	"errors"
	"fmt"

	"github.com/samperrin/pons"
)

// ClassifierCapability identifies a classifier provided by a trusted plugin.
func ClassifierCapability(id string) string { return "classifier:" + id }

// ClassifierPlugin makes a classifier available to policy plugins by ID.
type ClassifierPlugin struct {
	ID         string
	Classifier Classifier
}

func (p ClassifierPlugin) Setup(c *pons.Core) error {
	if p.ID == "" {
		return errors.New("actionpolicy: classifier plugin ID is required")
	}
	if p.Classifier == nil {
		return fmt.Errorf("actionpolicy: classifier %q is nil", p.ID)
	}
	return c.RegisterCapability(ClassifierCapability(p.ID), p.Classifier)
}
