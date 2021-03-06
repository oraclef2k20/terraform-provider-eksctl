package cluster

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/mumoshu/terraform-provider-eksctl/pkg/awsclicompat"
	"github.com/mumoshu/terraform-provider-eksctl/pkg/resource/cluster/metrics"
	"golang.org/x/sync/errgroup"
	"log"
	"os"
	"sync"
	"text/template"
	"time"
)

type CanaryOpts struct {
	CanaryAdvancementInterval time.Duration
	CanaryAdvancementStep     int
}

type GlobalMetricQueryTemplateData struct {
	ClusterName string
}

func graduallyShiftTraffic(set *ClusterSet, opts CanaryOpts) error {
	svc := elbv2.New(awsclicompat.NewSession(set.Cluster.Region))

	listenerStatuses := set.ListenerStatuses

	m := &ALBRouter{ELBV2: svc}

	{
		var err error

		m.Analyzers, err = MetricsToAnalyzers(set.Cluster.Region, set.Cluster.Metrics)
		if err != nil {
			return err
		}
	}

	return m.SwitchTargetGroup(CanaryConfig{Region: set.Cluster.Region, ClusterName: string(set.ClusterName)}, listenerStatuses, opts)
}

func MetricsToAnalyzers(region string, ms []Metric) ([]*Analyzer, error) {
	var analyzers []*Analyzer

	for _, m := range ms {
		var provider MetricProvider

		var err error

		switch m.Provider {
		case "cloudwatch":
			s := awsclicompat.NewSession(region)
			c := cloudwatch.New(s)
			provider = metrics.NewCloudWatchProvider(c, metrics.ProviderOpts{
				Address:  m.Address,
				Interval: 1 * time.Minute,
			})
		case "datadog":
			provider, err = metrics.NewDatadogProvider(metrics.ProviderOpts{
				Address:  m.Address,
				Interval: 1 * time.Minute,
			}, metrics.DatadogOpts{
				APIKey:         os.Getenv("DATADOG_API_KEY"),
				ApplicationKey: os.Getenv("DATADOG_APPLICATION_KEY"),
			})
		default:
			return nil, fmt.Errorf("creating metrics provider: unknown and unsupported provider %q specified", m.Provider)
		}

		if err != nil {
			return nil, fmt.Errorf("creating metrics provider %q: %v", m.Provider, err)
		}

		analyzers = append(analyzers, &Analyzer{
			MetricProvider: provider,
			Query:          m.Query,
			Min:            m.Min,
			Max:            m.Max,
		})
	}

	return analyzers, nil
}

func ListerStatusToTemplateData(l ListenerStatus) interface{} {
	targetGroupARN := *l.DesiredTG.TargetGroupArn
	var loadBalancerARNs []string

	for _, a := range l.DesiredTG.LoadBalancerArns {
		loadBalancerARNs = append(loadBalancerARNs, *a)
	}

	data := struct {
		TargetGroupARN   string
		LoadBalancerARNs []string
	}{
		TargetGroupARN:   targetGroupARN,
		LoadBalancerARNs: loadBalancerARNs,
	}

	return data
}

type MetricProvider interface {
	Execute(string) (float64, error)
}

type Analyzer struct {
	MetricProvider
	Query string
	Min   *float64
	Max   *float64
}

func (a *Analyzer) Analyze(data interface{}) error {
	maxRetries := 3

	var v float64

	var err error

	var query string

	{
		tmpl, err := template.New("query").Parse(a.Query)
		if err != nil {
			return fmt.Errorf("parsing query template: %w", err)
		}

		var buf bytes.Buffer

		if err := tmpl.Execute(&buf, data); err != nil {
			return fmt.Errorf("executing query template: %w", err)
		}

		query = buf.String()
	}

	for i := 0; i < maxRetries; i++ {
		v, err = a.MetricProvider.Execute(query)
		if err == nil {
			break
		}
	}

	if err != nil {
		return err
	}

	if a.Min != nil && *a.Min > v {
		return fmt.Errorf("checking value against threshold: %v is below %v", v, *a.Min)
	}

	if a.Max != nil && *a.Max < v {
		return fmt.Errorf("checking value against threshold: %v is beyond %v", v, *a.Max)
	}

	return nil
}

type ALBRouter struct {
	ELBV2 elbv2iface.ELBV2API

	Analyzers []*Analyzer
}

type CanaryConfig struct {
	Region      string
	ClusterName string
}

func (m *ALBRouter) SwitchTargetGroup(conf CanaryConfig, listenerStatuses ListenerStatuses, opts CanaryOpts) error {
	region := conf.Region

	svc := m.ELBV2

	setDesiredTGTrafficPercentage := func(l ListenerStatus, p int) error {
		if p > 100 {
			return fmt.Errorf("BUG: invalid value for p: got %d, must be less than 100", p)
		}

		if l.DesiredTG == nil {
			return fmt.Errorf("BUG: DesiredTG is nil: %+v", l)
		}

		if l.CurrentTG == nil {
			return fmt.Errorf("BUG: CurrentTG is nil: %+v", l)
		}

		if l.Rule == nil {
			return fmt.Errorf("BUG: Rule is nil: %+v", l)
		}

		_, err := svc.ModifyRule(&elbv2.ModifyRuleInput{
			Actions: []*elbv2.Action{
				{
					ForwardConfig: &elbv2.ForwardActionConfig{
						TargetGroupStickinessConfig: nil,
						TargetGroups: []*elbv2.TargetGroupTuple{
							{
								TargetGroupArn: l.DesiredTG.TargetGroupArn,
								Weight:         aws.Int64(int64(p)),
							}, {
								TargetGroupArn: l.CurrentTG.TargetGroupArn,
								Weight:         aws.Int64(int64(100 - p)),
							},
						},
					},
					Order: aws.Int64(1),
					Type:  aws.String("forward"),
				},
			},
			RuleArn: l.Rule.RuleArn,
		})
		if err != nil {
			return err
		}

		return nil
	}

	if len(listenerStatuses) == 0 {
		return nil
	}

	DefaultAnalyzeInterval := 10 * time.Second

	tCtx, cancel := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(tCtx)

	wg := &sync.WaitGroup{}

	for i := range listenerStatuses {
		l := listenerStatuses[i]

		var analyzers []*Analyzer
		{
			var err error

			analyzers, err = MetricsToAnalyzers(region, l.Metrics)
			if err != nil {
				return err
			}
		}

		if l.Rule.Actions != nil && len(l.Rule.Actions) > 0 {
			if len(l.Rule.Actions) != 1 {
				return fmt.Errorf("unexpected number of actions in rule %q: want 2, got %d", *l.Rule.RuleArn, len(l.Rule.Actions))
			}

			// Gradually shift traffic from current tg to desired tg by
			// updating rule
			var step int

			if opts.CanaryAdvancementStep > 0 {
				step = opts.CanaryAdvancementStep
			} else {
				step = 5
			}

			var advancementInterval time.Duration

			if opts.CanaryAdvancementInterval != 0 {
				advancementInterval = opts.CanaryAdvancementInterval
			} else {
				advancementInterval = 30 * time.Second
			}

			wg.Add(1)
			g.Go(func() error {
				ticker := time.NewTicker(advancementInterval)
				defer ticker.Stop()
				defer wg.Done()

				p := 1

				for {
					select {
					case <-ticker.C:
						if p >= 100 {
							fmt.Printf("Done.")
							p = 100

							if err := setDesiredTGTrafficPercentage(l, 100); err != nil {
								return err
							}
							return nil
						}

						if err := setDesiredTGTrafficPercentage(l, p); err != nil {
							return err
						}

						p += step
					case <-gctx.Done():
						if p != 100 {
							log.Printf("Rolling back traffic for listener %s", *l.Listener.ListenerArn)

							if err := setDesiredTGTrafficPercentage(l, 0); err != nil {
								return err
							}

							break
						}

						// Shouldn't this be `return nil`?
						return gctx.Err()
					}
				}

				log.Printf("Rolling back traffic shift for rule %s on listener %s", i, *l.Listener.ListenerArn)

				return nil
			})

			// Check per alb, per target group metrics
			for i := range analyzers {
				a := m.Analyzers[i]

				// TODO Check Datadog metrics and return non-nil error on check failure to cancel all the traffic shift
				g.Go(func() error {
					ticker := time.NewTicker(DefaultAnalyzeInterval)
					defer ticker.Stop()

					for {
						select {
						case <-gctx.Done():
							// Deployment finished. Stop checking as not necessary anymore
							return nil
						case <-ticker.C:
							if err := a.Analyze(ListerStatusToTemplateData(l)); err != nil {
								return err
							}
						}
					}
				})
			}

		}
	}

	// Check per cluster metrics
	for i := range m.Analyzers {
		a := m.Analyzers[i]

		g.Go(func() error {
			ticker := time.NewTicker(DefaultAnalyzeInterval)
			defer ticker.Stop()

			for {
				select {
				case <-gctx.Done():
					// Deployment finished. Stop checking as not necessary anymore
					return nil
				case <-ticker.C:
					if err := a.Analyze(GlobalMetricQueryTemplateData{ClusterName: conf.ClusterName}); err != nil {
						return err
					}
				}
			}
		})
	}

	go func() {
		defer cancel()

		wg.Wait()
	}()

	var err error
	{
		defer cancel()

		err = g.Wait()
	}

	if err == nil {
		log.Printf("Traffic shifting finished successfully.")
	} else if err == context.Canceled {
		log.Printf("Traffic shifting canceled externally.")

		return err
	} else {
		log.Printf("Traffic shifting canceled due to error: %w", err)

		return err
	}

	return nil
}

type ALBMetricsProvider struct {
}

type DatadogMetricsProvider struct {
}
