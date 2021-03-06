package main

import (
	"embed"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jessevdk/go-flags"
	"github.com/kennygrant/sanitize"
	log "github.com/sirupsen/logrus"

	"github.com/natesales/bcg/internal/bird"
	"github.com/natesales/bcg/internal/config"
	"github.com/natesales/bcg/internal/templating"
)

var version = "dev" // set by the build process

// PeeringDbResponse contains the response from a PeeringDB query
type PeeringDbResponse struct {
	Data []PeeringDbData `json:"data"`
}

// PeeringDbData contains the actual data from PeeringDB response
type PeeringDbData struct {
	Name    string `json:"name"`
	AsSet   string `json:"irr_as_set"`
	MaxPfx4 uint   `json:"info_prefixes4"`
	MaxPfx6 uint   `json:"info_prefixes6"`
}

// Config constants
const (
	DefaultIPv4TableSize = 1000000
	DefaultIPv6TableSize = 150000
)

// Flags
var opts struct {
	ConfigFile       string `short:"c" long:"config" description:"Configuration file in YAML, TOML, or JSON format" default:"/etc/bcg/config.yml"`
	Output           string `short:"o" long:"output" description:"Directory to write output files to" default:"/etc/bird/"`
	Socket           string `short:"s" long:"socket" description:"BIRD control socket" default:"/run/bird/bird.ctl"`
	KeepalivedConfig string `short:"k" long:"keepalived-config" description:"Configuration file for keepalived" default:"/etc/keepalived/keepalived.conf"`
	UiFile           string `short:"u" long:"ui-file" description:"File to store web UI" default:"/tmp/bcg-ui.html"`
	NoUi             bool   `short:"n" long:"no-ui" description:"Don't generate web UI"`
	Verbose          bool   `short:"v" long:"verbose" description:"Show verbose log messages"`
	DryRun           bool   `short:"d" long:"dry-run" description:"Don't modify BIRD config"`
	NoConfigure      bool   `long:"no-configure" description:"Don't configure BIRD"`
	ShowVersion      bool   `long:"version" description:"Show version and exit"`
}

// Embedded filesystem

//go:embed templates/*
var embedFs embed.FS

// Query PeeringDB for an ASN
func getPeeringDbData(asn uint) PeeringDbData {
	httpClient := http.Client{Timeout: time.Second * 5}
	req, err := http.NewRequest(http.MethodGet, "https://peeringdb.com/api/net?asn="+strconv.Itoa(int(asn)), nil)
	if err != nil {
		log.Fatalf("PeeringDB GET (This peer might not have a PeeringDB page): %v", err)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		log.Fatalf("PeeringDB GET Request: %v", err)
	}

	if res.Body != nil {
		//noinspection GoUnhandledErrorResult
		defer res.Body.Close()
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		log.Fatalf("PeeringDB Read: %v", err)
	}

	var peeringDbResponse PeeringDbResponse
	if err := json.Unmarshal(body, &peeringDbResponse); err != nil {
		log.Fatalf("PeeringDB JSON Unmarshal: %v", err)
	}

	if len(peeringDbResponse.Data) < 1 {
		log.Fatalf("Peer %d doesn't have a valid PeeringDB entry. Try import-valid or ask the network to update their account.", asn)
	}

	return peeringDbResponse.Data[0]
}

// Use bgpq4 to generate a prefix filter and return only the filter lines
func getPrefixFilter(asSet string, family uint8, irrdb string) []string {
	// Run bgpq4 for BIRD format with aggregation enabled
	log.Infof("Running bgpq4 -h %s -Ab%d %s", irrdb, family, asSet)
	cmd := exec.Command("bgpq4", "-h", irrdb, "-Ab"+strconv.Itoa(int(family)), asSet)
	stdout, err := cmd.Output()
	if err != nil {
		log.Fatalf("bgpq4 error: %v", err.Error())
	}

	// Remove whitespace and commas from output
	output := strings.ReplaceAll(string(stdout), ",\n    ", "\n")

	// Remove array prefix
	output = strings.ReplaceAll(output, "NN = [\n    ", "")

	// Remove array suffix
	output = strings.ReplaceAll(output, "];", "")

	// Check for empty IRR
	if output == "" {
		log.Warnf("Peer with as-set %s has no IPv%d prefixes. Disabled IPv%d connectivity.", asSet, family, family)
		return []string{}
	}

	// Remove whitespace (in this case there should only be trailing whitespace)
	output = strings.TrimSpace(output)

	// Split output by newline
	return strings.Split(output, "\n")
}

// Normalize a string to be filename-safe
func normalize(input string) string {
	// Remove non-alphanumeric characters
	input = sanitize.Path(input)

	// Make uppercase
	input = strings.ToUpper(input)

	// Replace spaces with underscores
	input = strings.ReplaceAll(input, " ", "_")

	// Replace slashes with dashes
	input = strings.ReplaceAll(input, "/", "-")

	return input
}

func main() {
	// Parse cli flags
	_, err := flags.ParseArgs(&opts, os.Args)
	if err != nil {
		if !strings.Contains(err.Error(), "Usage") {
			log.Fatal(err)
		}
		os.Exit(1)
	}

	// Enable debug logging in development releases
	if //noinspection GoBoolExpressions
	version == "devel" || opts.Verbose {
		log.SetLevel(log.DebugLevel)
	}

	if opts.ShowVersion {
		log.Printf("bcg version %s (https://github.com/natesales/bcg)\n", version)
		os.Exit(0)
	}

	log.Infof("Starting bcg %s", version)

	// Parse template files

	err = templating.Load(embedFs)
	if err != nil {
		log.Fatal(err)
	}

	log.Debug("Finished loading templates")

	// Load the config file from configFilename flag
	log.Debugf("Loading config from %s", opts.ConfigFile)
	globalConfig, err := config.Load(opts.ConfigFile)
	if err != nil {
		log.Fatal(err)
	}

	if len(globalConfig.Prefixes) == 0 {
		log.Info("There are no origin prefixes defined")
	} else {
		log.Debug("Building origin sets")

		// Assemble originIPv{4,6} lists by address family
		var originIPv4, originIPv6 []string
		for _, prefix := range globalConfig.Prefixes {
			if strings.Contains(prefix, ":") {
				originIPv6 = append(originIPv6, prefix)
			} else {
				originIPv4 = append(originIPv4, prefix)
			}
		}

		log.Debug("Finished building origin sets")
		log.Debug("OriginIPv4: ", originIPv4)
		log.Debug("OriginIPv6: ", originIPv6)

		globalConfig.OriginSet4 = originIPv4
		globalConfig.OriginSet6 = originIPv6
	}

	if !opts.DryRun {
		// Create the global output file
		log.Debug("Creating global config")
		globalFile, err := os.Create(path.Join(opts.Output, "bird.conf"))
		if err != nil {
			log.Fatalf("Create global BIRD output file: %v", err)
		}
		log.Debug("Finished creating global config file")

		// Render the global template and write to disk
		log.Debug("Writing global config file")
		err = templating.GlobalTemplate.ExecuteTemplate(globalFile, "global.tmpl", globalConfig)
		if err != nil {
			log.Fatalf("Execute global template: %v", err)
		}
		log.Debug("Finished writing global config file")

		// Remove old peer-specific configs
		files, err := filepath.Glob(path.Join(opts.Output, "AS*.conf"))
		if err != nil {
			panic(err)
		}
		for _, f := range files {
			if err := os.Remove(f); err != nil {
				log.Fatalf("Removing old config files: %v", err)
			}
		}
	} else {
		log.Info("Dry run is enabled, skipped writing global config and removing old peer configs")
	}

	// Iterate over peers
	for peerName, peerData := range globalConfig.Peers {
		// Add peer prefix if the first character of peerName is a number
		_peerName := strings.ReplaceAll(normalize(peerName), "-", "_")
		if unicode.IsDigit(rune(_peerName[0])) {
			_peerName = "PEER_" + _peerName
		}

		// Set normalized peer name
		peerData.Name = _peerName

		// Set default query time
		peerData.QueryTime = "[No operations performed]"

		log.Infof("Checking config for %s AS%d", peerName, peerData.Asn)

		// Validate peer type
		if !(peerData.Type == "upstream" || peerData.Type == "peer" || peerData.Type == "downstream" || peerData.Type == "import-valid") {
			log.Fatalf("[%s] type attribute is invalid. Must be upstream, peer, downstream, or import-valid", peerName)
		}

		log.Infof("[%s] type: %s", peerName, peerData.Type)

		// Only query PeeringDB and IRRDB for peers and downstreams, TODO: This should validate upstreams too
		if peerData.Type == "peer" || peerData.Type == "downstream" {
			peerData.QueryTime = time.Now().Format(time.RFC1123)
			peeringDbData := getPeeringDbData(peerData.Asn)

			if peerData.ImportLimit4 == 0 {
				peerData.ImportLimit4 = peeringDbData.MaxPfx4
				log.Infof("[%s] has no IPv4 import limit configured. Setting to %d from PeeringDB", peerName, peeringDbData.MaxPfx4)
			}

			if peerData.ImportLimit6 == 0 {
				peerData.ImportLimit6 = peeringDbData.MaxPfx6
				log.Infof("[%s] has no IPv6 import limit configured. Setting to %d from PeeringDB", peerName, peeringDbData.MaxPfx6)
			}

			// Only set AS-SET from PeeringDB if it isn't configure manually
			if peerData.AsSet == "" {
				// If the as-set has a space in it, split and pick the first element
				if strings.Contains(peeringDbData.AsSet, " ") {
					peeringDbData.AsSet = strings.Split(peeringDbData.AsSet, " ")[0]
					log.Warnf("[%s] has a space in their PeeringDB as-set field. Selecting first element %s", peerName, peeringDbData.AsSet)
				}

				// Trim IRRDB prefix
				if strings.Contains(peeringDbData.AsSet, "::") {
					peerData.AsSet = strings.Split(peeringDbData.AsSet, "::")[1]
					log.Warnf("[%s] has a IRRDB prefix in their PeeringDB as-set field. Using %s", peerName, peerData.AsSet)
				} else {
					peerData.AsSet = peeringDbData.AsSet
				}

				if peeringDbData.AsSet == "" {
					log.Fatalf("[%s] has no as-set in PeeringDB", peerName)
				} else {
					log.Infof("[%s] has no manual AS-SET defined. Setting to %s from PeeringDB\n", peerName, peeringDbData.AsSet)
				}
			} else {
				log.Infof("[%s] has manual AS-SET: %s", peerName, peerData.AsSet)
			}

			peerData.PrefixSet4 = getPrefixFilter(peerData.AsSet, 4, globalConfig.IrrDb)
			peerData.PrefixSet6 = getPrefixFilter(peerData.AsSet, 6, globalConfig.IrrDb)

			// Update the "latest operation" timestamp
			peerData.QueryTime = time.Now().Format(time.RFC1123)
		} else if peerData.Type == "upstream" || peerData.Type == "import-valid" {
			// Check for a zero prefix import limit
			if peerData.ImportLimit4 == 0 {
				peerData.ImportLimit4 = DefaultIPv4TableSize
				log.Infof("[%s] has no IPv4 import limit configured. Setting to %d", peerName, DefaultIPv4TableSize)
			}

			if peerData.ImportLimit6 == 0 {
				peerData.ImportLimit6 = DefaultIPv6TableSize
				log.Infof("[%s] has no IPv6 import limit configured. Setting to %d", peerName, DefaultIPv6TableSize)
			}
		}

		// If as-set is empty and the peer type requires it
		if peerData.AsSet == "" && (peerData.Type == "peer" || peerData.Type == "downstream") {
			log.Fatal("[%s] has no AS-SET defined and filtering profile requires it.", peerName)
		}

		// Print peer info
		log.Infof("[%s] local pref: %d", peerName, peerData.LocalPref)
		log.Infof("[%s] max prefixes: IPv4 %d, IPv6 %d", peerName, peerData.ImportLimit4, peerData.ImportLimit6)
		log.Infof("[%s] export-default: %v", peerName, peerData.ExportDefault)
		log.Infof("[%s] no-specifics: %v", peerName, peerData.NoSpecifics)
		log.Infof("[%s] allow-blackholes: %v", peerName, peerData.AllowBlackholes)

		if len(peerData.Communities) > 0 {
			log.Infof("[%s] communities: %s", peerName, strings.Join(peerData.Communities, ", "))
		}

		if len(peerData.LargeCommunities) > 0 {
			log.Infof("[%s] large-communities: %s", peerName, strings.Join(peerData.LargeCommunities, ", "))
		}

		// Check for additional options
		if peerData.AsSet != "" {
			log.Infof("[%s] as-set: %s", peerName, peerData.AsSet)
		}

		if peerData.Prepends > 0 {
			log.Infof("[%s] prepends: %d", peerName, peerData.Prepends)
		}

		if peerData.Multihop {
			log.Infof("[%s] multihop", peerName)
		}

		if peerData.Passive {
			log.Infof("[%s] passive", peerName)
		}

		if peerData.Disabled {
			log.Infof("[%s] disabled", peerName)
		}

		if peerData.EnforceFirstAs {
			log.Infof("[%s] enforce-first-as", peerName)
		}

		if peerData.EnforcePeerNexthop {
			log.Infof("[%s] enforce-peer-nexthop", peerName)
		}

		// Log neighbor IPs
		log.Infof("[%s] neighbors: %s", peerName, strings.Join(peerData.NeighborIPs, ", "))

		if !opts.DryRun {
			// Create the peer specific file
			peerSpecificFile, err := os.Create(path.Join(opts.Output, "AS"+strconv.Itoa(int(peerData.Asn))+"_"+normalize(peerName)+".conf"))
			if err != nil {
				log.Fatalf("Create peer specific output file: %v", err)
			}

			// Render the template and write to disk
			log.Infof("[%s] Writing config", peerName)
			err = templating.PeerTemplate.ExecuteTemplate(peerSpecificFile, "peer.tmpl", &config.Wrapper{Peer: *peerData, Config: *globalConfig})
			if err != nil {
				log.Fatalf("Execute template: %v", err)
			}

			log.Infof("[%s] Wrote config", peerName)
		} else {
			log.Infof("Dry run is enabled, skipped writing peer config(s)")
		}
	}

	// Write VRRP config
	if !opts.DryRun && len(globalConfig.VRRPInstances) > 0 {
		// Create the peer specific file
		peerSpecificFile, err := os.Create(path.Join(opts.KeepalivedConfig))
		if err != nil {
			log.Fatalf("Create peer specific output file: %v", err)
		}

		// Render the template and write to disk
		err = templating.VRRPTemplate.ExecuteTemplate(peerSpecificFile, "vrrp.tmpl", globalConfig.VRRPInstances)
		if err != nil {
			log.Fatalf("Execute template: %v", err)
		}
	} else {
		log.Infof("Dry run is enabled, not writing VRRP config")
	}

	if !opts.DryRun {
		if !opts.NoUi {
			// Create the ui output file
			log.Debug("Creating global config")
			uiFileObj, err := os.Create(opts.UiFile)
			if err != nil {
				log.Fatalf("Create UI output file: %v", err)
			}
			log.Debug("Finished creating UI file")

			// Render the UI template and write to disk
			log.Debug("Writing ui file")
			err = templating.UiTemplate.ExecuteTemplate(uiFileObj, "ui.tmpl", globalConfig)
			if err != nil {
				log.Fatalf("Execute ui template: %v", err)
			}
			log.Debug("Finished writing ui file")
		}

		if !opts.NoConfigure {
			log.Infoln("reconfiguring bird")
			if err = bird.RunCommand("configure", opts.Socket); err != nil {
				log.Fatal(err)
			}
		} else {
			log.Infoln("noreconfig is set, NOT reconfiguring bird")
		}
	}
}
