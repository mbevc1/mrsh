package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mbevc1/mrsh/pkg/config"
	"github.com/spf13/cobra"
)

var hostsCmd = &cobra.Command{
	Use:   "hosts",
	Short: "Manage the hosts config file",
}

// ---- hosts init ----

var hostsInitCmd = &cobra.Command{
	Use:     "init",
	Aliases: []string{"i"},
	Short:   "Create a new hosts.yaml config file interactively",
	RunE:    runHostsInit,
}

func init() {
	rootCmd.AddCommand(hostsCmd)
	hostsCmd.AddCommand(hostsInitCmd)
	hostsCmd.AddCommand(hostsListCmd)
	hostsCmd.AddCommand(hostsAddCmd)
	hostsCmd.AddCommand(hostsUpdateCmd)
	hostsCmd.AddCommand(hostsRemoveCmd)

	hostsListCmd.Flags().BoolVar(&showSecrets, "show-secrets", false, "Reveal masked passwords and sensitive fields")
	hostsAddCmd.Flags().StringVar(&addName, "name", "", "Host entry name")
	hostsAddCmd.Flags().StringVar(&addAddress, "address", "", "Host address or IP")
	hostsAddCmd.Flags().StringVar(&addGroup, "group", "", "Host group")
	hostsAddCmd.Flags().StringVar(&addUser, "user", "", "SSH user")
	hostsAddCmd.Flags().StringVar(&addPass, "pass", "", "SSH password or secret reference")
	hostsAddCmd.Flags().IntVar(&addPort, "port", 0, "SSH port (default 22)")
	hostsAddCmd.Flags().StringVar(&addKeyFile, "key-file", "", "Path to SSH private key")

	hostsUpdateCmd.Flags().StringVar(&addName, "name", "", "Name of host to update")
	hostsUpdateCmd.Flags().StringVar(&addAddress, "address", "", "New host address")
	hostsUpdateCmd.Flags().StringVar(&addGroup, "group", "", "New host group")
	hostsUpdateCmd.Flags().StringVar(&addUser, "user", "", "New SSH user")
	hostsUpdateCmd.Flags().StringVar(&addPass, "pass", "", "New SSH password or secret reference")
	hostsUpdateCmd.Flags().IntVar(&addPort, "port", 0, "New SSH port")
	hostsUpdateCmd.Flags().StringVar(&addKeyFile, "key-file", "", "New SSH private key path")

	hostsRemoveCmd.Flags().StringVar(&removeName, "name", "", "Name of host to remove")
	_ = hostsRemoveCmd.MarkFlagRequired("name")
}

func runHostsInit(cmd *cobra.Command, args []string) error {
	r := bufio.NewReader(os.Stdin)

	fmt.Println("mrsh config initializer")
	fmt.Println("=======================")

	configPath := prompt(r, "Config file path", "hosts.yaml")
	defaultUser := prompt(r, "Default SSH user", "admin")
	defaultPortStr := prompt(r, "Default SSH port", "22")
	defaultPort, _ := strconv.Atoi(defaultPortStr)
	if defaultPort == 0 {
		defaultPort = 22
	}
	defaultKeyFile := prompt(r, "Default SSH identity file (leave blank for password auth)", "~/.ssh/id_rsa")

	useSops := promptBool(r, "Enable SOPS encryption for sensitive fields?", false)

	rc := &config.RawConfig{
		Defaults: map[string]interface{}{
			"user":          defaultUser,
			"port":          defaultPort,
			"identity_file": defaultKeyFile,
			"timeout":       30,
		},
		Hosts: []map[string]interface{}{
			{
				"name":  "example",
				"host":  "192.168.1.1",
				"group": "default",
			},
		},
	}
	if err := rc.Write(configPath); err != nil {
		return err
	}
	fmt.Printf("Created %s\n", configPath)

	if useSops {
		ageKey := prompt(r, "age public key (age1...)", "")
		if ageKey != "" {
			sopsYAML := fmt.Sprintf(`creation_rules:
  - path_regex: hosts.*\.yaml$
    age: %s
    encrypted_regex: '^(pass|user)$'
`, ageKey)
			if err := os.WriteFile(".sops.yaml", []byte(sopsYAML), 0600); err != nil {
				return fmt.Errorf("write .sops.yaml: %w", err)
			}
			fmt.Println("Created .sops.yaml")
		}
		fmt.Printf("\nTo encrypt sensitive fields, run:\n  sops -e -i %s\n", configPath)
	}

	return nil
}

// ---- hosts list ----

var showSecrets bool

var hostsListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "l"},
	Short:   "List all hosts in the config",
	RunE:    runHostsList,
}

func runHostsList(cmd *cobra.Command, args []string) error {
	if err := loadConfig(); err != nil {
		return err
	}
	hosts := filteredHosts()
	if len(hosts) == 0 {
		fmt.Println("No hosts found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tADDRESS\tGROUP\tUSER\tPORT\tPASS\tKEY_FILE")
	for _, h := range hosts {
		pass := h.Pass
		if !showSecrets && pass != "" {
			pass = "***"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			h.Name, h.Address, h.Group, h.User, h.Port, pass, h.KeyFile)
	}
	return w.Flush()
}

// ---- hosts add ----

var (
	addName    string
	addAddress string
	addGroup   string
	addUser    string
	addPass    string
	addPort    int
	addKeyFile string
)

var hostsAddCmd = &cobra.Command{
	Use:     "add",
	Aliases: []string{"a"},
	Short:   "Add a new host entry to the config",
	RunE:    runHostsAdd,
}

func runHostsAdd(cmd *cobra.Command, args []string) error {
	// hosts add operates on the raw YAML file — no secret resolution needed.
	if addName == "" {
		return fmt.Errorf("--name is required")
	}
	if addAddress == "" {
		return fmt.Errorf("--address is required")
	}

	raw, err := config.ReadRaw(cfgFile)
	if err != nil {
		return err
	}

	// Check for duplicate name.
	for _, h := range raw.Hosts {
		if h["name"] == addName {
			return fmt.Errorf("host %q already exists", addName)
		}
	}

	entry := map[string]interface{}{
		"name": addName,
		"host": addAddress,
	}
	if addGroup != "" {
		entry["group"] = addGroup
	}
	if addUser != "" {
		entry["user"] = addUser
	}
	if addPass != "" {
		entry["pass"] = addPass
	}
	if addPort != 0 {
		entry["port"] = addPort
	}
	if addKeyFile != "" {
		entry["identity_file"] = addKeyFile
	}
	raw.Hosts = append(raw.Hosts, entry)

	if err := raw.Write(cfgFile); err != nil {
		return err
	}
	fmt.Printf("Updated %s\n", cfgFile)
	return nil
}

// ---- hosts update ----

var hostsUpdateCmd = &cobra.Command{
	Use:     "update",
	Aliases: []string{"up", "u"},
	Short:   "Update an existing host entry in the config",
	RunE:    runHostsUpdate,
}

func runHostsUpdate(cmd *cobra.Command, args []string) error {
	if addName == "" {
		return fmt.Errorf("--name is required")
	}

	raw, err := config.ReadRaw(cfgFile)
	if err != nil {
		return err
	}

	found := false
	for i, h := range raw.Hosts {
		if h["name"] == addName {
			found = true
			if addAddress != "" {
				raw.Hosts[i]["host"] = addAddress
			}
			if addGroup != "" {
				raw.Hosts[i]["group"] = addGroup
			}
			if addUser != "" {
				raw.Hosts[i]["user"] = addUser
			}
			if addPass != "" {
				raw.Hosts[i]["pass"] = addPass
			}
			if addPort != 0 {
				raw.Hosts[i]["port"] = addPort
			}
			if addKeyFile != "" {
				raw.Hosts[i]["identity_file"] = addKeyFile
			}
			break
		}
	}
	if !found {
		return fmt.Errorf("host %q not found", addName)
	}

	if err := raw.Write(cfgFile); err != nil {
		return err
	}
	fmt.Printf("Updated %s\n", cfgFile)
	return nil
}

// ---- hosts remove ----

var removeName string

var hostsRemoveCmd = &cobra.Command{
	Use:     "remove",
	Aliases: []string{"rm", "r"},
	Short:   "Remove a host entry from the config",
	RunE:    runHostsRemove,
}

func runHostsRemove(cmd *cobra.Command, args []string) error {
	raw, err := config.ReadRaw(cfgFile)
	if err != nil {
		return err
	}

	idx := -1
	for i, h := range raw.Hosts {
		if h["name"] == removeName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("host %q not found", removeName)
	}

	r := bufio.NewReader(os.Stdin)
	answer := prompt(r, fmt.Sprintf("Remove host %q? [y/N]", removeName), "N")
	if !strings.EqualFold(strings.TrimSpace(answer), "y") {
		fmt.Println("Aborted.")
		return nil
	}

	raw.Hosts = append(raw.Hosts[:idx], raw.Hosts[idx+1:]...)
	if err := raw.Write(cfgFile); err != nil {
		return err
	}
	fmt.Printf("Removed host %q from %s\n", removeName, cfgFile)
	return nil
}

// ---- helpers ----

// prompt prints a prompt with a default value and reads a line from r.
func prompt(r *bufio.Reader, question, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", question, defaultVal)
	} else {
		fmt.Printf("%s: ", question)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return defaultVal
	}
	return line
}

func promptBool(r *bufio.Reader, question string, defaultVal bool) bool {
	d := "n"
	if defaultVal {
		d = "y"
	}
	answer := prompt(r, question+" [y/N]", d)
	return strings.EqualFold(strings.TrimSpace(answer), "y")
}
