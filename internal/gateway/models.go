package gateway

import (
	"context"
	"errors"
	"log"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/site"
)

// modelsTTL is how long the site's model list is kept.
//
// 模型的名单一年也换不了几回。客户端问一次就让它跑一趟 MiniMax，是把
// 一个本地能回答的东西做成了网络调用。
const modelsTTL = 10 * time.Minute

// modelsFallbackTTL is how long the built-in list is kept when the
// site could not be reached.
//
// 比真的那份短：站点暂时答不上来，过一会儿就该再问一次。
const modelsFallbackTTL = time.Minute

// modelsTimeout is how long the site has to answer the model list.
const modelsTimeout = 5 * time.Second

// liveModels reads the site's own model list.
//
// 问的时候不带账号的令牌：站点没有令牌也答，而带令牌去问有可能在路上
// 把令牌续了 —— 一份模型名单不值得换一把新令牌。
func (s *Server) liveModels(ctx context.Context) ([]site.Model, error) {
	if s.Mgr == nil || s.Mgr.Up == nil {
		return nil, errors.New("models: there is no upstream client")
	}
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	body, err := s.Mgr.Up.Models(ctx, s.UpstreamBase, "")
	if err != nil {
		return nil, err
	}
	ms := account.ParseModels(body)
	if len(ms) == 0 {
		return nil, errors.New("models: the site's answer has no models in it")
	}
	return ms, nil
}

// modelsFor gives the models the site has.
//
// 站点的那份读到了就留着，读不到就摆内置的那份：内置的那份是站点已
// 知有的东西，比一份空名单好。
func (s *Server) modelsFor(ctx context.Context) []site.Model {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()

	if s.models != nil {
		ttl := modelsTTL
		if !s.modelsLive {
			ttl = modelsFallbackTTL
		}
		if time.Since(s.modelsAt) < ttl {
			return s.models
		}
	}

	ms, err := s.liveModels(ctx)
	if err != nil {
		log.Printf("models: the site's list could not be read: %v", err)
		s.models = site.FallbackModels()
		s.modelsLive = false
		s.modelsAt = time.Now()
		return s.models
	}
	s.models = ms
	s.modelsLive = true
	s.modelsAt = time.Now()
	return ms
}

// modelEntry gives one model as the API tells it.
//
// magpie 还要 context/output、能不能读图、思考档位，所以这些字段都
// 给上：别的客户端（OpenAI 形状的那些）会忽略多出来的字段。
func modelEntry(m site.Model) map[string]any {
	inputs := m.Inputs
	if len(inputs) == 0 {
		inputs = []string{"text"}
	}
	efforts := m.Efforts
	if efforts == nil {
		efforts = []string{}
	}
	return map[string]any{
		"id": m.ID, "object": "model", "owned_by": "minimax",
		"limit": map[string]any{
			"context": m.Context,
			"output":  m.Output,
		},
		"modalities": map[string]any{
			"input":  inputs,
			"output": []string{"text"},
		},
		"reasoning": m.Thinking != site.ThinkingOff,
		"thinking":  string(m.Thinking),
		"efforts":   efforts,
	}
}
