package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// --- THEME ---
var (
	cPrimary   = lipgloss.Color("#326CE5")
	cSecondary = lipgloss.Color("#D8DEE9")
	cDim       = lipgloss.Color("#687182")
	cSelection = lipgloss.Color("#3E4451")
	cBg        = lipgloss.Color("#2E3440")

	cGreen  = lipgloss.Color("#4CAF50")
	cOrange = lipgloss.Color("#FFC107")
	cRed    = lipgloss.Color("#F44336")
	cCyan   = lipgloss.Color("#00BCD4")

	headerStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(cPrimary).Padding(0, 1)
	statsStyle       = lipgloss.NewStyle().Foreground(cSecondary)
	contextStyle     = lipgloss.NewStyle().Foreground(cCyan).Bold(true)
	colHeadStyle     = lipgloss.NewStyle().Foreground(cDim).Bold(true)
	selectedRowStyle = lipgloss.NewStyle().Background(cSelection).Foreground(lipgloss.Color("#FFFFFF")).Bold(true)
	footerStyle      = lipgloss.NewStyle().Foreground(cDim)

	diagHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(cDim).Padding(0, 1)
	diagTitleStyle  = lipgloss.NewStyle().Foreground(cPrimary).Bold(true)
	modalStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cRed).Padding(1, 4).Align(lipgloss.Center)
)

// --- DATA ---
type PodInfo struct {
	Namespace string
	Name      string
	Ready     string
	Status    string
	Restarts  int32
	CpuUsage  string
	MemUsage  string
	NodeName  string
	PodIP     string
	IsReady   bool
	Message   string // The "NOTES" column data
}

type ClusterStats struct {
	TotalCpuUsage, TotalMemUsage, TotalCpuCap, TotalMemCap int64
	NodeCount                                              int
}

type sessionState int

const (
	viewList sessionState = iota
	viewLogs
	viewDiagnosis
	viewDeleteConfirm
)

type model struct {
	client        *kubernetes.Clientset
	metricsClient *metricsv.Clientset
	kubeconfig    string

	pods          []PodInfo
	filteredPods  []PodInfo
	clusterStats  ClusterStats
	namespaces    []string
	currentNsIdx  int
	state         sessionState
	cursor        int
	showIssues    bool
	loading       bool
	msg           string
	podToDelete   *PodInfo
	viewport      viewport.Model
	logContent    string
	diagContent   string
	selectedPod   *PodInfo
	width, height int
}

// --- INIT ---
func main() {
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) path to kubeconfig")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig")
	}
	flag.Parse()
	configPath := *kubeconfig
	if configPath == "" {
		configPath = os.Getenv("KUBECONFIG")
	}

	config, err := clientcmd.BuildConfigFromFlags("", configPath)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}
	metricsClient, err := metricsv.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	p := tea.NewProgram(initialModel(clientset, metricsClient, configPath), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error: %v", err)
		os.Exit(1)
	}
}

func initialModel(c *kubernetes.Clientset, m *metricsv.Clientset, k string) model {
	return model{client: c, metricsClient: m, kubeconfig: k, state: viewList, loading: true, namespaces: []string{"ALL"}}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchPods(m.client, m.metricsClient), fetchClusterStats(m.client, m.metricsClient), fetchNamespaces(m.client), tick())
}

// --- UPDATE ---
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport = viewport.New(msg.Width, msg.Height-10)

	case tea.KeyMsg:
		switch m.state {
		case viewList:
			switch msg.String() {
			case "q", "ctrl+c":
				return m, tea.Quit
			case "up", "k":
				if m.cursor > 0 {
					m.cursor--
				}
			case "down", "j":
				if m.cursor < len(m.filteredPods)-1 {
					m.cursor++
				}
			case "n":
				if len(m.namespaces) > 1 {
					m.currentNsIdx++
					if m.currentNsIdx >= len(m.namespaces) {
						m.currentNsIdx = 0
					}
					m.cursor = 0
					m.filterPods()
				}
			case "tab":
				m.showIssues = !m.showIssues
				m.cursor = 0
				m.filterPods()
				if m.showIssues {
					m.msg = "Filter: Issues Only"
				} else {
					m.msg = "Filter: Showing All"
				}
			case "enter":
				if len(m.filteredPods) > 0 {
					selected := m.filteredPods[m.cursor]
					m.selectedPod = &selected
					m.state = viewLogs
					m.msg = fmt.Sprintf("Logs: %s", selected.Name)
					return m, fetchLogs(m.client, selected)
				}
			case "?":
				if len(m.filteredPods) > 0 {
					selected := m.filteredPods[m.cursor]
					m.selectedPod = &selected
					m.state = viewDiagnosis
					m.msg = fmt.Sprintf("Diagnosing %s...", selected.Name)
					return m, diagnosePod(m.client, selected)
				}
			case "d":
				if len(m.filteredPods) > 0 {
					selected := m.filteredPods[m.cursor]
					m.podToDelete = &selected
					m.state = viewDeleteConfirm
				}
			case "s":
				if len(m.filteredPods) > 0 {
					selected := m.filteredPods[m.cursor]
					return m, openShell(selected.Namespace, selected.Name, m.kubeconfig)
				}
			}

		case viewDeleteConfirm:
			switch msg.String() {
			case "y", "Y":
				m.msg = fmt.Sprintf("Deleting %s...", m.podToDelete.Name)
				cmd = deletePod(m.client, *m.podToDelete)
				m.podToDelete = nil
				m.state = viewList
				return m, cmd
			case "n", "N", "esc", "q":
				m.podToDelete = nil
				m.state = viewList
				m.msg = "Delete cancelled."
			}
		case viewLogs, viewDiagnosis:
			switch msg.String() {
			case "esc", "q":
				m.state = viewList
				m.msg = "Dashboard"
			default:
				m.viewport, cmd = m.viewport.Update(msg)
				return m, cmd
			}
		}

	case tickMsg:
		return m, tea.Batch(fetchPods(m.client, m.metricsClient), fetchClusterStats(m.client, m.metricsClient), tick())
	case podsMsg:
		m.pods = msg
		m.loading = false
		m.filterPods()
		if m.cursor >= len(m.filteredPods) {
			if len(m.filteredPods) > 0 {
				m.cursor = len(m.filteredPods) - 1
			} else {
				m.cursor = 0
			}
		}
	case statsMsg:
		m.clusterStats = ClusterStats(msg)
	case nsMsg:
		m.namespaces = append([]string{"ALL"}, msg...)
	case logsMsg:
		m.logContent = string(msg)
		m.viewport.SetContent(m.logContent)
		m.viewport.GotoBottom()
	case diagMsg:
		m.diagContent = string(msg)
		m.viewport.SetContent(m.diagContent)
		m.viewport.GotoTop()
	case deleteMsg:
		m.msg = string(msg)
		return m, fetchPods(m.client, m.metricsClient)
	}
	return m, nil
}

func (m *model) filterPods() {
	var target []PodInfo
	selectedNs := m.namespaces[m.currentNsIdx]
	for _, p := range m.pods {
		if selectedNs != "ALL" && p.Namespace != selectedNs {
			continue
		}
		if m.showIssues {
			if (p.Status == "Running" || p.Status == "Succeeded") && p.Restarts == 0 && p.IsReady {
				continue
			}
		}
		target = append(target, p)
	}
	m.filteredPods = target
}

// --- VIEW ---
func (m model) View() string {
	if m.width == 0 {
		return "Loading..."
	}
	if m.state == viewDeleteConfirm {
		return m.deleteConfirmView()
	}
	if m.state == viewLogs {
		return m.logsView()
	}
	if m.state == viewDiagnosis {
		return m.diagnosisView()
	}

	title := headerStyle.Render(" KUBE-PULSE PRO ")
	cpuPerc := 0
	memPerc := 0
	if m.clusterStats.TotalCpuCap > 0 {
		cpuPerc = int((float64(m.clusterStats.TotalCpuUsage) / float64(m.clusterStats.TotalCpuCap)) * 100)
	}
	if m.clusterStats.TotalMemCap > 0 {
		memPerc = int((float64(m.clusterStats.TotalMemUsage) / float64(m.clusterStats.TotalMemCap)) * 100)
	}
	stats := statsStyle.Render(fmt.Sprintf("  Nodes: %d  |  CPU: %d%%  |  MEM: %d%%", m.clusterStats.NodeCount, cpuPerc, memPerc))
	topBar := fmt.Sprintf("%s%s", title, stats)

	var contextInfo string
	currentNs := m.namespaces[m.currentNsIdx]
	if len(m.filteredPods) > 0 && m.cursor < len(m.filteredPods) {
		sel := m.filteredPods[m.cursor]
		contextInfo = contextStyle.Render(fmt.Sprintf("  NS: %s  |  NODE: %s  |  IP: %s", currentNs, sel.NodeName, sel.PodIP))
	} else {
		contextInfo = lipgloss.NewStyle().Foreground(cDim).Render(fmt.Sprintf("  NS: %s  |  No pods found.", currentNs))
	}

	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	// NOTES column added
	fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", "NAMESPACE", "NAME", "RDY", "STATUS", "RST", "CPU", "MEM", "NOTES")

	start, end := m.calculatePagination()
	for i := start; i < end; i++ {
		p := m.filteredPods[i]
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t\n",
			truncate(p.Namespace, 18),
			truncate(p.Name, 30),
			p.Ready,
			p.Status,
			p.Restarts,
			p.CpuUsage,
			p.MemUsage,
			truncate(p.Message, 25),
		)
	}
	w.Flush()

	rawTable := b.String()
	lines := strings.Split(rawTable, "\n")
	var styledRows string
	if len(lines) > 0 {
		styledRows += colHeadStyle.Render(lines[0]) + "\n"
	}

	lineIdx := 1
	for i := start; i < end; i++ {
		if lineIdx >= len(lines) {
			break
		}
		rawLine := lines[lineIdx]
		if rawLine == "" {
			lineIdx++
			continue
		}
		p := m.filteredPods[i]
		rowStyle := lipgloss.NewStyle().Foreground(cSecondary)

		if i == m.cursor {
			rowStyle = selectedRowStyle
			if len(rawLine) > 2 {
				rawLine = "| " + rawLine[2:]
			}
		} else {
			// FIXED SYNTAX ERROR HERE
			if (p.Status != "Running" && p.Status != "Succeeded") || !p.IsReady {
				rowStyle = rowStyle.Foreground(cRed)
			} else if p.Restarts > 0 {
				rowStyle = rowStyle.Foreground(cOrange)
			}
		}
		styledRows += rowStyle.Render(rawLine) + "\n"
		lineIdx++
	}

	help := footerStyle.Render(fmt.Sprintf("\n  [Tab] Filter (%v)  [n] NS  [?] Doctor  [s] Shell  [d] Delete  [q] Quit", m.showIssues))
	status := lipgloss.NewStyle().Foreground(cPrimary).Padding(0, 2).Render(m.msg)

	return "\n" + topBar + "\n\n" + contextInfo + "\n\n" + styledRows + "\n" + help + "\n" + status
}

func (m model) deleteConfirmView() string {
	box := modalStyle.Render(fmt.Sprintf("%s\n\nConfirm deletion of:\n%s\n\n%s / %s", lipgloss.NewStyle().Foreground(cRed).Bold(true).Render("[!] DELETE POD"), lipgloss.NewStyle().Foreground(cSecondary).Render(m.podToDelete.Name), lipgloss.NewStyle().Foreground(cGreen).Bold(true).Render("[y] Confirm"), lipgloss.NewStyle().Foreground(cDim).Render("[n] Cancel")))
	return strings.Repeat("\n", m.height/3) + lipgloss.PlaceHorizontal(m.width, lipgloss.Center, box)
}
func (m model) logsView() string {
	return "\n" + headerStyle.Render(" LOGS: "+m.selectedPod.Name) + "\n\n" + m.viewport.View() + "\n\n" + footerStyle.Render("  [Esc] Back")
}
func (m model) diagnosisView() string {
	return "\n" + diagHeaderStyle.Render(" [DIAGNOSIS]: "+m.selectedPod.Name) + "\n\n" + m.viewport.View() + "\n\n" + footerStyle.Render("  [Esc] Back")
}
func openShell(namespace, pod, kubeconfig string) tea.Cmd {
	return tea.ExecProcess(exec.Command("kubectl", "exec", "-it", "-n", namespace, pod, "--", "/bin/sh", "-c", "bash || sh"), func(err error) tea.Msg { return nil })
}

// --- HELPERS & ASYNC ---
func (m model) calculatePagination() (int, int) {
	perPage := m.height - 12
	if perPage <= 0 {
		perPage = 5
	}
	start, end := 0, len(m.filteredPods)
	if len(m.filteredPods) > perPage {
		if m.cursor < perPage {
			end = perPage
		} else {
			start = m.cursor - perPage + 1
			end = m.cursor + 1
		}
	}
	return start, end
}
func truncate(s string, l int) string {
	if len(s) > l {
		return s[:l-2] + ".."
	}
	return s
}
func tick() tea.Cmd { return tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }

type tickMsg time.Time
type podsMsg []PodInfo
type statsMsg ClusterStats
type nsMsg []string
type logsMsg string
type diagMsg string
type deleteMsg string

func fetchNamespaces(c *kubernetes.Clientset) tea.Cmd {
	return func() tea.Msg {
		l, e := c.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
		if e != nil {
			return nil
		}
		var n []string
		for _, i := range l.Items {
			n = append(n, i.Name)
		}
		sort.Strings(n)
		return nsMsg(n)
	}
}

func fetchPods(c *kubernetes.Clientset, m *metricsv.Clientset) tea.Cmd {
	return func() tea.Msg {
		pList, e := c.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
		if e != nil {
			return nil
		}
		uMap := make(map[string]corev1.ResourceList)
		mList, _ := m.MetricsV1beta1().PodMetricses("").List(context.TODO(), metav1.ListOptions{})
		if mList != nil {
			for _, i := range mList.Items {
				cT, mT := resource.Quantity{}, resource.Quantity{}
				for _, c := range i.Containers {
					cT.Add(*c.Usage.Cpu())
					mT.Add(*c.Usage.Memory())
				}
				uMap[i.Namespace+"/"+i.Name] = corev1.ResourceList{corev1.ResourceCPU: cT, corev1.ResourceMemory: mT}
			}
		}

		var list []PodInfo
		for _, p := range pList.Items {
			r := int32(0)
			ready := 0
			total := len(p.Status.ContainerStatuses)
			msg := "[OK]"

			for _, c := range p.Status.ContainerStatuses {
				r += c.RestartCount
				if c.Ready {
					ready++
				}
				if c.State.Waiting != nil && c.State.Waiting.Reason != "" {
					msg = c.State.Waiting.Reason
				} else if c.State.Terminated != nil && c.State.Terminated.Reason != "" {
					msg = c.State.Terminated.Reason
					if c.State.Terminated.ExitCode != 0 {
						msg = fmt.Sprintf("%s (%d)", msg, c.State.Terminated.ExitCode)
					}
				}
			}

			if p.Status.Phase == "Running" && ready == total {
				msg = "[OK]"
			}
			if p.Status.Phase == "Succeeded" {
				msg = "Completed"
			}

			cStr, mStr := "-", "-"
			if u, ok := uMap[p.Namespace+"/"+p.Name]; ok {
				cStr = fmt.Sprintf("%dm", u.Cpu().MilliValue())
				mStr = fmt.Sprintf("%dMi", u.Memory().Value()/(1024*1024))
			}
			isReady := (ready == total && total > 0) || (p.Status.Phase == "Succeeded")
			readyStr := fmt.Sprintf("%d/%d", ready, total)

			list = append(list, PodInfo{
				Namespace: p.Namespace, Name: p.Name, Ready: readyStr, Status: string(p.Status.Phase),
				Restarts: r, CpuUsage: cStr, MemUsage: mStr, NodeName: p.Spec.NodeName, PodIP: p.Status.PodIP,
				IsReady: isReady, Message: msg,
			})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Status != "Running" && list[j].Status == "Running" {
				return true
			}
			if list[i].Status == "Running" && list[j].Status != "Running" {
				return false
			}
			return list[i].Name < list[j].Name
		})
		return podsMsg(list)
	}
}
func fetchClusterStats(c *kubernetes.Clientset, m *metricsv.Clientset) tea.Cmd {
	return func() tea.Msg {
		nodes, err := c.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return statsMsg(ClusterStats{})
		}
		totalCpuCap := int64(0)
		totalMemCap := int64(0)
		for _, n := range nodes.Items {
			totalCpuCap += n.Status.Allocatable.Cpu().MilliValue()
			totalMemCap += n.Status.Allocatable.Memory().Value()
		}
		nodeMetrics, _ := m.MetricsV1beta1().NodeMetricses().List(context.TODO(), metav1.ListOptions{})
		totalCpuUse := int64(0)
		totalMemUse := int64(0)
		if nodeMetrics != nil {
			for _, nm := range nodeMetrics.Items {
				totalCpuUse += nm.Usage.Cpu().MilliValue()
				totalMemUse += nm.Usage.Memory().Value()
			}
		}
		return statsMsg(ClusterStats{TotalCpuUsage: totalCpuUse, TotalMemUsage: totalMemUse, TotalCpuCap: totalCpuCap, TotalMemCap: totalMemCap, NodeCount: len(nodes.Items)})
	}
}
func fetchLogs(c *kubernetes.Clientset, p PodInfo) tea.Cmd {
	return func() tea.Msg {
		req := c.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{TailLines: func(i int64) *int64 { return &i }(100)})
		stream, err := req.Stream(context.TODO())
		if err != nil {
			return logsMsg("Error fetching logs")
		}
		defer stream.Close()
		buf := new(bytes.Buffer)
		io.Copy(buf, stream)
		return logsMsg(buf.String())
	}
}
func deletePod(c *kubernetes.Clientset, p PodInfo) tea.Cmd {
	return func() tea.Msg {
		c.CoreV1().Pods(p.Namespace).Delete(context.TODO(), p.Name, metav1.DeleteOptions{})
		return deleteMsg("Pod deleted.")
	}
}
func diagnosePod(client *kubernetes.Clientset, pod PodInfo) tea.Cmd {
	return func() tea.Msg {
		events, err := client.CoreV1().Events(pod.Namespace).List(context.TODO(), metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", pod.Name)})
		var report strings.Builder
		report.WriteString(diagTitleStyle.Render("[EVENTS]") + "\n")
		if err == nil && len(events.Items) > 0 {
			for _, e := range events.Items {
				if e.Type == "Warning" {
					report.WriteString(fmt.Sprintf("* %s: %s\n", lipgloss.NewStyle().Foreground(cRed).Render(e.Reason), e.Message))
				}
			}
		} else {
			report.WriteString("No critical events.\n")
		}
		report.WriteString("\n" + diagTitleStyle.Render("[ANALYSIS]") + "\n")
		if pod.Restarts > 5 {
			report.WriteString("[!] High Restarts: App likely crashing on init.\n")
		}
		if pod.Status == "Pending" {
			report.WriteString("[!] Pending: Check Node Capacity / PVC.\n")
		}
		if !pod.IsReady && pod.Status == "Running" {
			report.WriteString("[!] Running but Not Ready: Readiness probe failed or app starting.\n")
		}
		report.WriteString("\n" + diagTitleStyle.Render("[LOGS]") + "\n")
		req := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: func(i int64) *int64 { return &i }(15)})
		stream, _ := req.Stream(context.TODO())
		if stream != nil {
			defer stream.Close()
			buf := new(bytes.Buffer)
			io.Copy(buf, stream)
			report.WriteString(lipgloss.NewStyle().Foreground(cDim).Render(buf.String()))
		}
		return diagMsg(report.String())
	}
}
