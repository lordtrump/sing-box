package route

import (
	"context"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/x/list"

	"go4.org/netipx"
)

var _ adapter.RuleSet = (*LocalRuleSet)(nil)

type LocalRuleSet struct {
	tag      string
	abstractRuleSet
	refs     atomic.Int32
}

func NewLocalRuleSet(ctx context.Context, router adapter.Router, options option.RuleSet) (*LocalRuleSet, error) {
	ctx, cancel := context.WithCancel(ctx)
	ruleSet := LocalRuleSet{
		abstractRuleSet: abstractRuleSet{
			ctx:    ctx,
			cancel: cancel,
			tag:    options.Tag,
			pType:  "local",
			path:   options.Path,
			format: options.Format,
		},
	}
	return &ruleSet, ruleSet.loadFromFile(router, true)
}

func (s *LocalRuleSet) Update(router adapter.Router) error {
	return s.loadFromFile(router, false)
}

func (s *LocalRuleSet) Name() string {
	return s.tag
}

func (s *LocalRuleSet) StartContext(ctx context.Context, startContext adapter.RuleSetStartContext) error {
	return nil
}

func (s *LocalRuleSet) PostStart() error {
	return nil
}

func (s *LocalRuleSet) ExtractIPSet() []*netipx.IPSet {
	return common.FlatMap(s.rules, extractIPSetFromRule)
}

func (s *LocalRuleSet) IncRef() {
	s.refs.Add(1)
}

func (s *LocalRuleSet) DecRef() {
	if s.refs.Add(-1) < 0 {
		panic("rule-set: negative refs")
	}
}

func (s *LocalRuleSet) Cleanup() {
	if s.refs.Load() == 0 {
		s.rules = nil
	}
}

func (s *LocalRuleSet) RegisterCallback(callback adapter.RuleSetUpdateCallback) *list.Element[adapter.RuleSetUpdateCallback] {
	return nil
}

func (s *LocalRuleSet) UnregisterCallback(element *list.Element[adapter.RuleSetUpdateCallback]) {
}

func (s *LocalRuleSet) Close() error {
	s.cancel()
	s.rules = nil
	return nil
}
