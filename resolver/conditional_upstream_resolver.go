package resolver

import (
	"strings"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/util"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

// ConditionalUpstreamResolver delegates DNS question to other DNS resolver dependent on domain name in question
type ConditionalUpstreamResolver struct {
	configurable[*config.ConditionalUpstreamConfig]
	NextResolver
	typed

	mapping map[string]Resolver
}

// NewConditionalUpstreamResolver returns new resolver instance
func NewConditionalUpstreamResolver(
	cfg config.ConditionalUpstreamConfig, bootstrap *Bootstrap, shouldVerifyUpstreams bool,
) (*ConditionalUpstreamResolver, error) {
	m := make(map[string]Resolver, len(cfg.Mapping.Upstreams))

	for domain, upstream := range cfg.Mapping.Upstreams {
		pbCfg := config.UpstreamsConfig{
			Groups: config.UpstreamGroups{
				upstreamDefaultCfgName: upstream,
			},
		}

		r, err := NewParallelBestResolver(pbCfg, bootstrap, shouldVerifyUpstreams)
		if err != nil {
			return nil, err
		}

		m[strings.ToLower(domain)] = r
	}

	r := ConditionalUpstreamResolver{
		configurable: withConfig(&cfg),
		typed:        withType("conditional_upstream"),

		mapping: m,
	}

	return &r, nil
}

func (r *ConditionalUpstreamResolver) processRequest(request *model.Request) (bool, *model.Response, error) {
	domainFromQuestion := util.ExtractDomain(request.Req.Question[0])
	domain := domainFromQuestion

	if strings.Contains(domainFromQuestion, ".") {
		// try with domain with and without sub-domains
		for len(domain) > 0 {
			if resolver, found := r.mapping[domain]; found {
				resp, err := r.internalResolve(resolver, domainFromQuestion, domain, request)

				return true, resp, err
			}

			if i := strings.Index(domain, "."); i >= 0 {
				domain = domain[i+1:]
			} else {
				break
			}
		}
	} else if resolver, found := r.mapping["."]; found {
		resp, err := r.internalResolve(resolver, domainFromQuestion, domain, request)

		return true, resp, err
	}

	return false, nil, nil
}

// Resolve uses the conditional resolver to resolve the query
func (r *ConditionalUpstreamResolver) Resolve(request *model.Request) (*model.Response, error) {
	logger := log.WithPrefix(request.Log, "conditional_resolver")

	if len(r.mapping) > 0 {
		resolved, resp, err := r.processRequest(request)
		if resolved {
			return resp, err
		}
	}

	logger.WithField("next_resolver", Name(r.next)).Trace("go to next resolver")

	return r.next.Resolve(request)
}

func (r *ConditionalUpstreamResolver) internalResolve(reso Resolver, doFQ, do string,
	req *model.Request,
) (*model.Response, error) {
	// internal request resolution
	logger := log.WithPrefix(req.Log, "conditional_resolver")

	req.Req.Question[0].Name = dns.Fqdn(doFQ)
	response, err := reso.Resolve(req)

	if err == nil {
		response.Reason = "CONDITIONAL"
		response.RType = model.ResponseTypeCONDITIONAL

		if len(response.Res.Question) > 0 {
			response.Res.Question[0].Name = req.Req.Question[0].Name
		}
	}

	var answer string
	if response != nil {
		answer = util.AnswerToString(response.Res.Answer)
	}

	logger.WithFields(logrus.Fields{
		"answer":   answer,
		"domain":   util.Obfuscate(do),
		"upstream": reso,
	}).Debugf("received response from conditional upstream")

	return response, err
}
