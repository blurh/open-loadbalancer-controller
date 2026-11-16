package lvs

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type commandDriver struct {
	binary  string
	timeout time.Duration
}

func newCommandDriver(binary string, timeout time.Duration) (*commandDriver, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("find ipvsadm: %w", err)
	}
	return &commandDriver{binary: path, timeout: timeout}, nil
}

func (d *commandDriver) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.binary, args...)
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ipvsadm %s: %w", strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("ipvsadm %s: %w: %.2048s", strings.Join(args, " "), err, output)
	}
	return output, nil
}

func serviceArgs(action string, key listener) []string {
	protocol := "-t"
	if key.Protocol == "udp" {
		protocol = "-u"
	}
	return []string{action, protocol, key.Address}
}
func (d *commandDriver) Service(action string, rule virtualService) error {
	args := serviceArgs(action, rule.listener)
	if action != "-D" {
		args = append(args, "-s", rule.Scheduler)
	}
	_, err := d.run(args...)
	return err
}
func (d *commandDriver) Destination(action string, key listener, dst destination) error {
	args := append(serviceArgs(action, key), "-r", dst.Address)
	if action != "-d" {
		flag := map[string]string{"nat": "-m", "dr": "-g", "tun": "-i"}[dst.Mode]
		args = append(args, flag, "-w", strconv.Itoa(dst.Weight))
	}
	_, err := d.run(args...)
	return err
}
func (d *commandDriver) List() (map[listener]virtualService, error) {
	output, err := d.run("-S", "-n")
	if err != nil {
		return nil, err
	}
	return parseSave(string(output))
}

// parseSave 只解析 TCP/UDP 地址规则。fwmark/SCTP 等规则由其他程序管理，保持原样。
// 未识别的托管规则选项标记 Unsupported，避免下一次同步破坏外部配置。
func parseSave(data string) (map[listener]virtualService, error) {
	result := make(map[listener]virtualService)
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if fields[0] == "ipvsadm" {
			fields = fields[1:]
		}
		if len(fields) < 3 || (fields[0] != "-A" && fields[0] != "-a") {
			return nil, fmt.Errorf("invalid ipvsadm save line: %q", line)
		}
		if fields[1] != "-t" && fields[1] != "-u" {
			continue
		}
		// IPVS 也可能有其他程序创建的 IPv6 和通配端口规则，只按原始 key 记录占用。
		key := listener{Protocol: "tcp", Address: fields[2]}
		if fields[1] == "-u" {
			key.Protocol = "udp"
		}
		if normalized, err := ipv4Endpoint(key.Address); err == nil {
			key.Address = normalized
		}
		rule, exists := result[key]
		if fields[0] == "-A" {
			if exists {
				return nil, fmt.Errorf("duplicate IPVS service %v", key)
			}
			rule = virtualService{listener: key}
		} else if !exists {
			return nil, fmt.Errorf("IPVS destination without service: %q", line)
		}
		dst := destination{Mode: "dr", Weight: 1}
		for i := 3; i < len(fields); i++ {
			switch fields[i] {
			case "-s", "-r", "-w":
				if i+1 == len(fields) {
					return nil, fmt.Errorf("truncated IPVS option: %q", line)
				}
				option, value := fields[i], fields[i+1]
				i++
				switch option {
				case "-s":
					rule.Scheduler = value
				case "-r":
					dst.Address = value
				case "-w":
					weight, err := strconv.Atoi(value)
					if err != nil || weight < 0 || weight > 65535 {
						return nil, fmt.Errorf("invalid IPVS weight: %q", line)
					}
					dst.Weight = weight
				}
			case "-m":
				dst.Mode = "nat"
			case "-g":
				dst.Mode = "dr"
			case "-i":
				dst.Mode = "tun"
			default:
				rule.Unsupported = true
			}
		}
		if fields[0] == "-a" {
			if dst.Address == "" {
				return nil, fmt.Errorf("IPVS destination address missing: %q", line)
			}
			rule.Destinations = append(rule.Destinations, dst)
		} else if rule.Scheduler == "" {
			return nil, fmt.Errorf("IPVS scheduler missing: %q", line)
		}
		result[key] = rule
	}
	return result, scanner.Err()
}
