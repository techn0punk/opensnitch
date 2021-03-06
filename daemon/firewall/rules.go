package firewall

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/evilsocket/opensnitch/daemon/core"
	"github.com/evilsocket/opensnitch/daemon/log"
)

// DropMark is the mark we place on a connection when we deny it.
// The connection is dropped later on OUTPUT chain.
const DropMark = 0x18BA5

// Action is the modifier we apply to a rule.
type Action string

// Actions we apply to the firewall.
const (
	ADD      = Action("-A")
	INSERT   = Action("-I")
	DELETE   = Action("-D")
	FLUSH    = Action("-F")
	NEWCHAIN = Action("-N")
	DELCHAIN = Action("-X")

	systemRulePrefix = "opensnitch-filter"
)

// make sure we don't mess with multiple rules
// at the same time
var (
	lock = sync.Mutex{}

	queueNum = 0
	running  = false
	// check that rules are loaded every 30s
	rulesChecker             = time.NewTicker(time.Second * 30)
	rulesCheckerChan         = make(chan bool)
	regexRulesQuery, _       = regexp.Compile(`NFQUEUE.*ctstate NEW,RELATED.*NFQUEUE num.*bypass`)
	regexDropQuery, _        = regexp.Compile(`DROP.*mark match 0x18ba5`)
	regexSystemRulesQuery, _ = regexp.Compile(systemRulePrefix + ".*")

	systemChains = make(map[string]*fwRule)
)

// RunRule inserts or deletes a firewall rule.
func RunRule(action Action, enable bool, logError bool, rule []string) error {
	if enable == false {
		action = "-D"
	}

	rule = append([]string{string(action)}, rule...)

	lock.Lock()
	defer lock.Unlock()

	if _, err := core.Exec("iptables", rule); err != nil {
		if logError {
			log.Error("Error while running firewall rule, ipv4 err: %s", err)
			log.Error("rule: %s", rule)
		}
		return err
	}

	if core.IPv6Enabled {
		if _, err := core.Exec("ip6tables", rule); err != nil {
			if logError {
				log.Error("Error while running firewall rule, ipv6 err: %s", err)
				log.Error("rule: %s", rule)
			}
			return err
		}
	}

	return nil
}

// QueueDNSResponses redirects DNS responses to us, in order to keep a cache
// of resolved domains.
// INPUT --protocol udp --sport 53 -j NFQUEUE --queue-num 0 --queue-bypass
func QueueDNSResponses(enable bool, logError bool, qNum int) (err error) {
	return RunRule(INSERT, enable, logError, []string{
		"INPUT",
		"--protocol", "udp",
		"--sport", "53",
		"-j", "NFQUEUE",
		"--queue-num", fmt.Sprintf("%d", qNum),
		"--queue-bypass",
	})
}

// QueueConnections inserts the firewall rule which redirects connections to us.
// They are queued until the user denies/accept them, or reaches a timeout.
// OUTPUT -t mangle -m conntrack --ctstate NEW,RELATED -j NFQUEUE --queue-num 0 --queue-bypass
func QueueConnections(enable bool, logError bool, qNum int) (err error) {
	return RunRule(INSERT, enable, logError, []string{
		"OUTPUT",
		"-t", "mangle",
		"-m", "conntrack",
		"--ctstate", "NEW,RELATED",
		"-j", "NFQUEUE",
		"--queue-num", fmt.Sprintf("%d", qNum),
		"--queue-bypass",
	})
}

// DropMarked rejects packets marked by OpenSnitch.
// OUTPUT -m mark --mark 101285 -j DROP
func DropMarked(enable bool, logError bool) (err error) {
	return RunRule(ADD, enable, logError, []string{
		"OUTPUT",
		"-m", "mark",
		"--mark", fmt.Sprintf("%d", DropMark),
		"-j", "DROP",
	})
}

// CreateSystemRule create the custom firewall chains and adds them to system.
func CreateSystemRule(rule *fwRule, logErrors bool) {
	chainName := systemRulePrefix + "-" + rule.Chain
	if _, ok := systemChains[rule.Table+"-"+chainName]; ok {
		return
	}
	RunRule(NEWCHAIN, true, logErrors, []string{chainName, "-t", rule.Table})

	// Insert the rule at the top of the chain
	if err := RunRule(INSERT, true, logErrors, []string{rule.Chain, "-t", rule.Table, "-j", chainName}); err == nil {
		systemChains[rule.Table+"-"+chainName] = rule
	}
}

// DeleteSystemRules deletes the system rules
func DeleteSystemRules(logErrors bool) {
	for _, r := range fwConfig.SystemRules {
		chain := systemRulePrefix + "-" + r.Rule.Chain
		if _, ok := systemChains[r.Rule.Table+"-"+chain]; !ok {
			continue
		}
		RunRule(FLUSH, true, logErrors, []string{chain, "-t", r.Rule.Table})
		RunRule(DELETE, false, logErrors, []string{r.Rule.Chain, "-t", r.Rule.Table, "-j", chain})
		RunRule(DELCHAIN, true, logErrors, []string{chain, "-t", r.Rule.Table})
		delete(systemChains, r.Rule.Table+"-"+chain)
	}
}

// AddSystemRule inserts a new rule.
func AddSystemRule(action Action, rule *fwRule, enable bool) (err error) {
	chain := systemRulePrefix + "-" + rule.Chain
	if rule.Table == "" {
		rule.Table = "filter"
	}
	r := []string{chain, "-t", rule.Table}
	if rule.Parameters != "" {
		r = append(r, strings.Split(rule.Parameters, " ")...)
	}
	r = append(r, []string{"-j", rule.Target}...)
	if rule.TargetParameters != "" {
		r = append(r, strings.Split(rule.TargetParameters, " ")...)
	}

	return RunRule(action, enable, true, r)
}

// AreRulesLoaded checks if the firewall rules are loaded.
func AreRulesLoaded() bool {
	lock.Lock()
	defer lock.Unlock()

	var outDrop6 string
	var outMangle6 string

	outDrop, err := core.Exec("iptables", []string{"-n", "-L", "OUTPUT"})
	if err != nil {
		return false
	}
	outMangle, err := core.Exec("iptables", []string{"-n", "-L", "OUTPUT", "-t", "mangle"})
	if err != nil {
		return false
	}

	if core.IPv6Enabled {
		outDrop6, err = core.Exec("ip6tables", []string{"-n", "-L", "OUTPUT"})
		if err != nil {
			return false
		}
		outMangle6, err = core.Exec("ip6tables", []string{"-n", "-L", "OUTPUT", "-t", "mangle"})
		if err != nil {
			return false
		}
	}

	systemRulesLoaded := true
	if len(systemChains) > 0 {
		for _, rule := range systemChains {
			if chainOut4, err4 := core.Exec("iptables", []string{"-n", "-L", rule.Chain, "-t", rule.Table}); err4 == nil {
				if regexSystemRulesQuery.FindString(chainOut4) == "" {
					systemRulesLoaded = false
					break
				}
			}
			if core.IPv6Enabled {
				if chainOut6, err6 := core.Exec("ip6tables", []string{"-n", "-L", rule.Chain, "-t", rule.Table}); err6 == nil {
					if regexSystemRulesQuery.FindString(chainOut6) == "" {
						systemRulesLoaded = false
						break
					}
				}
			}
		}
	}

	result := regexDropQuery.FindString(outDrop) != "" &&
		regexRulesQuery.FindString(outMangle) != "" &&
		systemRulesLoaded

	if core.IPv6Enabled {
		result = result && regexDropQuery.FindString(outDrop6) != "" &&
			regexRulesQuery.FindString(outMangle6) != ""
	}

	return result
}

// StartCheckingRules checks periodically if the rules are loaded.
// If they're not, we insert them again.
func StartCheckingRules() {
	for {
		select {
		case <-rulesCheckerChan:
			goto Exit
		case <-rulesChecker.C:
			if rules := AreRulesLoaded(); rules == false {
				log.Important("firewall rules changed, reloading")
				CleanRules(log.GetLogLevel() == log.DEBUG)
				insertRules()
				loadDiskConfiguration(true)
			}
		}
	}

Exit:
	log.Info("exit checking fw rules")
}

// StopCheckingRules stops checking if the firewall rules are loaded.
func StopCheckingRules() {
	rulesChecker.Stop()
	rulesCheckerChan <- true
}

// IsRunning returns if the firewall rules are loaded or not.
func IsRunning() bool {
	return running
}

// CleanRules deletes the rules we added.
func CleanRules(logErrors bool) {
	QueueDNSResponses(false, logErrors, queueNum)
	QueueConnections(false, logErrors, queueNum)
	DropMarked(false, logErrors)
	DeleteSystemRules(logErrors)
}

func insertRules() {
	if err := QueueDNSResponses(true, true, queueNum); err != nil {
		log.Error("Error while running DNS firewall rule: %s", err)
	} else if err = QueueConnections(true, true, queueNum); err != nil {
		log.Fatal("Error while running conntrack firewall rule: %s", err)
	} else if err = DropMarked(true, true); err != nil {
		log.Fatal("Error while running drop firewall rule: %s", err)
	}
}

// Stop deletes the firewall rules, allowing network traffic.
func Stop(qNum *int) {
	if running == false {
		return
	}
	if qNum != nil {
		queueNum = *qNum
	}

	configWatcher.Close()
	StopCheckingRules()
	CleanRules(log.GetLogLevel() == log.DEBUG)

	running = false
}

// Init inserts the firewall rules.
func Init(qNum *int) {
	if running {
		return
	}
	if qNum != nil {
		queueNum = *qNum
	}
	insertRules()

	if watcher, err := fsnotify.NewWatcher(); err == nil {
		configWatcher = watcher
	}
	loadDiskConfiguration(false)

	go StartCheckingRules()

	running = true
}
