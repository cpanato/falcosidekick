package outputs

import (
	"context"
	"fmt"
	"log"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/falcosecurity/falcosidekick/types"
	"github.com/kubernetes-sigs/wg-policy-prototypes/policy-report/kube-bench-adapter/pkg/apis/wgpolicyk8s.io/v1alpha2"
	wgpolicy "github.com/kubernetes-sigs/wg-policy-prototypes/policy-report/kube-bench-adapter/pkg/apis/wgpolicyk8s.io/v1alpha2"
	crdClient "github.com/kubernetes-sigs/wg-policy-prototypes/policy-report/kube-bench-adapter/pkg/generated/v1alpha2/clientset/versioned"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

//for n namespace specific reports with their count
type pr struct {
	report *wgpolicy.PolicyReport
	count  int
}

const (
	clusterPolicyReportBaseName = "falco-cluster-policy-report"
	policyReportSource          = "Falco"
	highpri                     = "high"
)

var (
	failbound int
	//count for cluster policy report
	repcount int
	//slice of policyreports and their counts(type pr)
	polreports []pr
	//for cluster policyreport
	report *wgpolicy.ClusterPolicyReport = &wgpolicy.ClusterPolicyReport{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterPolicyReportBaseName,
		},
		Summary: v1alpha2.PolicyReportSummary{
			Fail: 0,
			Warn: 0, //to-do
		},
	}
)

func NewPolicyReportClient(config *types.Configuration, stats *types.Statistics, promStats *types.PromStatistics, statsdClient, dogstatsdClient *statsd.Client) (*Client, error) {
	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		clientConfig, err = clientcmd.BuildConfigFromFlags("", config.PolicyReport.Kubeconfig)
		if err != nil {
			fmt.Printf("[ERROR] :unable to load kube config file: %v", err)
		}
	}
	crdclient, err := crdClient.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}
	return &Client{
		OutputType:      "PolicyReport",
		Config:          config,
		Stats:           stats,
		PromStats:       promStats,
		StatsdClient:    statsdClient,
		DogstatsdClient: dogstatsdClient,
		Crdclient:       crdclient,
	}, nil
} //KubernetesClient: clientset,

// CreateReport creates PolicyReport/ClusterPolicyReport Resource in Kubernetes
func (c *Client) CreateReport(falcopayload types.FalcoPayload) {
	failbound = c.Config.PolicyReport.FailThreshold
	r, namespaceScoped := newResult(falcopayload)

	if namespaceScoped != "" {
		forPolicyReports(c, namespaceScoped, r)
	} else {
		forClusterPolicyReport(c, r)
	}
}

//newResult creates a new entry for Reports
func newResult(FalcoPayload types.FalcoPayload) (c *wgpolicy.PolicyReportResult, namespaceScoped string) {
	namespaceScoped = "" // decision variable to increment for policyreport and clusterpolicyreport //to do //false for clusterpolicyreport
	var m = make(map[string]string)
	for index, element := range FalcoPayload.OutputFields {
		if index == "ka.target.namespace" || index == "k8s.ns.name" {
			namespaceScoped = fmt.Sprintf("%v", element) //true for policyreport
		}
		m[index] = fmt.Sprintf("%v", element)
	}
	var pri string //initial hardcoded priority bounds
	if FalcoPayload.Priority > types.PriorityType(failbound) {
		pri = highpri
	} else if FalcoPayload.Priority < types.PriorityType(failbound) {
		pri = "low"
	} else {
		pri = "medium"
	}
	return &wgpolicy.PolicyReportResult{
		Policy:      FalcoPayload.Rule,
		Source:      policyReportSource,
		Scored:      false,
		Timestamp:   metav1.Timestamp{Seconds: int64(FalcoPayload.Time.Second()), Nanos: int32(FalcoPayload.Time.Nanosecond())},
		Severity:    v1alpha2.PolicyResultSeverity(pri),
		Result:      "fail",
		Description: FalcoPayload.Output,
		Properties:  m,
	}, namespaceScoped
}

//check for low priority events to delete first
func checklow(result []*wgpolicy.PolicyReportResult) (swapint int) {
	for i, j := range result {
		if j.Severity == "medium" || j.Severity == "low" {
			return i
		}
	}
	return -1
}

//check if policy report exists
func repexist(ns string) int {
	for i, j := range polreports {
		if j.report.ObjectMeta.Name == ns {
			return i
		}
	}
	return len(polreports)
}

//update summary for clusterpolicyreport 'report'
func updateClusterSummary(r *wgpolicy.PolicyReportResult) {
	if r.Severity == highpri {
		report.Summary.Fail++
	} else {
		report.Summary.Warn++
	}
}

//update summary for specific policyreport in 'polreports' at index 'n'
func updatePolicyReportSummary(n int, r *wgpolicy.PolicyReportResult) {
	if r.Severity == highpri {
		polreports[n].report.Summary.Fail++
	} else {
		polreports[n].report.Summary.Warn++
	}
}

func forPolicyReports(c *Client, namespace string, r *wgpolicy.PolicyReportResult) {
	repname := "falcoreport-" + namespace
	//find if the specific namespace report exists and assign its index to n
	n := repexist(repname)
	//policyreport to be created
	if n == len(polreports) {
		//n == len(slice) ; report doesnt exist so we append a new report to the slice
		var polreport *wgpolicy.PolicyReport = &wgpolicy.PolicyReport{
			ObjectMeta: metav1.ObjectMeta{
				Name: repname,
			},
			Summary: v1alpha2.PolicyReportSummary{
				Fail: 0,
				Warn: 0, //to-do
			},
		}
		toappend := pr{report: polreport, count: 0}
		polreports = append(polreports, toappend)
	}
	policyr := c.Crdclient.Wgpolicyk8sV1alpha2().PolicyReports(namespace)
	// fmt.Println(namespace)
	updatePolicyReportSummary(n, r)
	polreports[n].count++
	if polreports[n].count > c.Config.PolicyReport.MaxEvents {
		if c.Config.PolicyReport.PruneByPriority == true {
			pruningLogicForPolicyReports(n)
		} else {
			polreports[n].report.Results[0] = nil
			polreports[n].report.Results = polreports[n].report.Results[1:]
			polreports[n].count = polreports[n].count - 1
		}
	}
	polreports[n].report.Results = append(polreports[n].report.Results, r)
	_, getErr := policyr.Get(context.Background(), polreports[n].report.Name, metav1.GetOptions{})
	if errors.IsNotFound(getErr) {
		result, err := policyr.Create(context.TODO(), polreports[n].report, metav1.CreateOptions{})
		if err != nil {
			log.Printf("[ERROR] : %v\n", err)
		}
		fmt.Printf("[INFO] :Created policy-report %q in namespace: %v.\n", result.GetObjectMeta().GetName(), namespace)
	} else {
		// Update existing Policy Report
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, err := policyr.Get(context.Background(), polreports[n].report.GetName(), metav1.GetOptions{})
			if errors.IsNotFound(err) {
				// This doesnt ever happen even if it is already deleted or not found
				log.Printf("[ERROR] :%v not found", polreports[n].report.GetName())
				return nil
			}
			if err != nil {
				return err
			}
			polreports[n].report.SetResourceVersion(result.GetResourceVersion())
			_, updateErr := policyr.Update(context.Background(), polreports[n].report, metav1.UpdateOptions{})
			return updateErr
		})
		if retryErr != nil {
			fmt.Printf("[ERROR] :update failed: %v", retryErr)
		}
		fmt.Println("[INFO] :updated policy report...")
	}
}

func forClusterPolicyReport(c *Client, r *wgpolicy.PolicyReportResult) {
	updateClusterSummary(r)
	//clusterpolicyreport to be created
	clusterpr := c.Crdclient.Wgpolicyk8sV1alpha2().ClusterPolicyReports()

	repcount++
	if repcount > c.Config.PolicyReport.MaxEvents {
		//To do for pruning
		if c.Config.PolicyReport.PruneByPriority == true {
			pruningLogicForClusterReport()
		} else {
			report.Results[0] = nil
			report.Results = report.Results[1:]
			repcount = repcount - 1
		}
	}
	report.Results = append(report.Results, r)
	_, getErr := clusterpr.Get(context.Background(), report.Name, metav1.GetOptions{})
	if errors.IsNotFound(getErr) {
		result, err := clusterpr.Create(context.TODO(), report, metav1.CreateOptions{})
		if err != nil {
			log.Printf("[ERROR] : %v\n", err)
		}
		fmt.Printf("[INFO] :Created cluster-policy-report %q.\n", result.GetObjectMeta().GetName())
	} else {
		// Update existing Cluster Policy Report
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, err := clusterpr.Get(context.Background(), report.GetName(), metav1.GetOptions{})
			if errors.IsNotFound(err) {
				// This doesnt ever happen even if it is already deleted or not found
				log.Printf("[ERROR] :%v not found", report.GetName())
				return nil
			}
			if err != nil {
				return err
			}
			report.SetResourceVersion(result.GetResourceVersion())
			_, updateErr := clusterpr.Update(context.Background(), report, metav1.UpdateOptions{})
			return updateErr
		})
		if retryErr != nil {
			fmt.Printf("[ERROR] :update failed: %v", retryErr)
		}
		fmt.Println("[INFO] :updated cluster policy report...")
	}
}

func pruningLogicForPolicyReports(n int) {
	//To do for pruning for pruning one of policyreports
	checklowvalue := checklow(polreports[n].report.Results)
	if checklowvalue > 0 {
		polreports[n].report.Results[checklowvalue] = polreports[n].report.Results[0]
	}
	polreports[n].report.Results[0] = nil
	polreports[n].report.Results = polreports[n].report.Results[1:]
	polreports[n].count = polreports[n].count - 1
}

func pruningLogicForClusterReport() {
	//To do for pruning cluster report
	checklowvalue := checklow(report.Results)
	if checklowvalue > 0 {
		report.Results[checklowvalue] = report.Results[0]
	}
	report.Results[0] = nil
	report.Results = report.Results[1:]
	repcount = repcount - 1
}