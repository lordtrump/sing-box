package route

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/atomic"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/x/list"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"

	"go4.org/netipx"
)

var _ adapter.RuleSet = (*RemoteRuleSet)(nil)

type RemoteRuleSet struct {
	abstractRuleSet
	router         adapter.Router
	logger         logger.ContextLogger
	options        option.RemoteRuleSet
	lastUpdated    time.Time
	lastEtag       string
	updateInterval time.Duration
	dialer         N.Dialer
	updateTicker   *time.Ticker
	pauseManager   pause.Manager
	callbackAccess sync.Mutex
	callbacks      list.List[adapter.RuleSetUpdateCallback]
	refs           atomic.Int32
}

func NewRemoteRuleSet(ctx context.Context, router adapter.Router, logger logger.ContextLogger, options option.RuleSet) *RemoteRuleSet {
	ctx, cancel := context.WithCancel(ctx)
	var updateInterval time.Duration
	if options.RemoteOptions.UpdateInterval > 0 {
		updateInterval = time.Duration(options.RemoteOptions.UpdateInterval)
	} else {
		updateInterval = 24 * time.Hour
	}
	return &RemoteRuleSet{
		abstractRuleSet: abstractRuleSet{
			ctx:    ctx,
			cancel: cancel,
			tag:    options.Tag,
			pType:  "remote",
			path:   options.Path,
			format: options.Format,
		},
		router:         router,
		logger:         logger,
		options:        options.RemoteOptions,
		lastEtag:       "",
		updateInterval: updateInterval,
		pauseManager:   service.FromContext[pause.Manager](ctx),
	}
}

func (s *RemoteRuleSet) Name() string {
	return s.abstractRuleSet.Tag()
}

func (s *RemoteRuleSet) Update(router adapter.Router) error {
	return s.fetchOnce(s.ctx, nil)
}

func (s *RemoteRuleSet) StartContext(ctx context.Context, startContext adapter.RuleSetStartContext) error {
	var dialer N.Dialer
	if s.options.DownloadDetour != "" {
		outbound, loaded := s.router.Outbound(s.options.DownloadDetour)
		if !loaded {
			return E.New("download_detour not found: ", s.options.DownloadDetour)
		}
		dialer = outbound
	} else {
		outbound, err := s.router.DefaultOutbound(N.NetworkTCP)
		if err != nil {
			return err
		}
		dialer = outbound
	}
	s.dialer = dialer
	s.loadFromFile(s.router, true)
	s.lastUpdated = s.updatedTime
	if s.lastUpdated.IsZero() {
		err := s.fetchOnce(ctx, startContext)
		if err != nil {
			return E.Cause(err, "initial rule-set: ", s.tag)
		}
	}
	s.updateTicker = time.NewTicker(1 * time.Minute)
	return nil
}

func (s *RemoteRuleSet) PostStart() error {
	go s.loopUpdate()
	return nil
}

func (s *RemoteRuleSet) ExtractIPSet() []*netipx.IPSet {
	return common.FlatMap(s.rules, extractIPSetFromRule)
}

func (s *RemoteRuleSet) IncRef() {
	s.refs.Add(1)
}

func (s *RemoteRuleSet) DecRef() {
	if s.refs.Add(-1) < 0 {
		panic("rule-set: negative refs")
	}
}

func (s *RemoteRuleSet) Cleanup() {
	if s.refs.Load() == 0 {
		s.rules = nil
	}
}

func (s *RemoteRuleSet) RegisterCallback(callback adapter.RuleSetUpdateCallback) *list.Element[adapter.RuleSetUpdateCallback] {
	s.callbackAccess.Lock()
	defer s.callbackAccess.Unlock()
	return s.callbacks.PushBack(callback)
}

func (s *RemoteRuleSet) UnregisterCallback(element *list.Element[adapter.RuleSetUpdateCallback]) {
	s.callbackAccess.Lock()
	defer s.callbackAccess.Unlock()
	s.callbacks.Remove(element)
}

func (s *RemoteRuleSet) loopUpdate() {
	if time.Since(s.lastUpdated) > s.updateInterval {
		err := s.fetchOnce(s.ctx, nil)
		if err != nil {
			s.logger.Error("fetch rule-set ", s.tag, ": ", err)
		} else if s.refs.Load() == 0 {
			s.rules = nil
		}
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.updateTicker.C:
			if time.Since(s.lastUpdated) < s.updateInterval {
				continue
			}
			s.pauseManager.WaitActive()
			err := s.fetchOnce(s.ctx, nil)
			if err != nil {
				s.logger.Error("fetch rule-set ", s.tag, ": ", err)
			} else if s.refs.Load() == 0 {
				s.rules = nil
			}
			runtime.GC()
		}
	}
}

func (s *RemoteRuleSet) fetchOnce(ctx context.Context, startContext adapter.RuleSetStartContext) error {
	s.logger.Debug("updating rule-set ", s.tag, " from URL: ", s.options.URL)
	var httpClient *http.Client
	if startContext != nil {
		httpClient = startContext.HTTPClient(s.options.DownloadDetour, s.dialer)
	} else {
		httpClient = &http.Client{
			Transport: &http.Transport{
				TLSHandshakeTimeout: C.TCPTimeout,
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return s.dialer.DialContext(ctx, network, M.ParseSocksaddr(addr))
				},
			},
		}
	}
	request, err := http.NewRequest("GET", s.options.URL, nil)
	if err != nil {
		return err
	}
	if s.lastEtag != "" {
		request.Header.Set("If-None-Match", s.lastEtag)
	}
	s.lastUpdated = time.Now()
	response, err := httpClient.Do(request.WithContext(ctx))
	if err != nil {
		return err
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		s.updatedTime = s.lastUpdated
		os.Chtimes(s.path, s.lastUpdated, s.lastUpdated)
		s.logger.Info("update rule-set ", s.tag, ": not modified")
		return nil
	default:
		return E.New("unexpected status: ", response.Status)
	}
	content, err := io.ReadAll(response.Body)
	if err != nil {
		response.Body.Close()
		return err
	}
	err = s.loadData(s.router, content)
	if err != nil {
		response.Body.Close()
		return err
	}
	response.Body.Close()
	eTagHeader := response.Header.Get("Etag")
	if eTagHeader != "" {
		s.lastEtag = eTagHeader
	}
	s.updatedTime = s.lastUpdated
	os.WriteFile(s.path, content, 0o666)
	s.logger.Info("update rule-set ", s.tag, " success")
	return nil
}

func (s *RemoteRuleSet) Close() error {
	s.rules = nil
	s.updateTicker.Stop()
	s.cancel()
	return nil
}
