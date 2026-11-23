package auth

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	aReset  = "\x1b[0m"
	aBold   = "\x1b[1m"
	aDim    = "\x1b[38;5;242m"
	aCyan   = "\x1b[38;5;111m"
	aGreen  = "\x1b[38;5;114m"
	aYellow = "\x1b[38;5;221m"
	aRed    = "\x1b[38;5;203m"
)

func runMenu(args []string) int {
	_, opts := parseReloadFlags(args)
	// In a popup, reload all idle agy panes (the popup itself is not an agy pane).
	opts.SkipPaneID = ""

	selected := 0
	statusMsg := ""

	for {
		accounts, err := List()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%serror:%s %v\n", aRed, aReset, err)
			time.Sleep(1500 * time.Millisecond)
			return 1
		}
		if len(accounts) > 0 && selected >= len(accounts) {
			selected = len(accounts) - 1
		}
		if selected < 0 {
			selected = 0
		}

		// Clear screen + home cursor
		fmt.Print("\x1b[2J\x1b[H")
		fmt.Printf("%s%s  agy-kit auth%s  %s(switch account & reload idle agy panes)%s\n", aBold, aCyan, aReset, aDim, aReset)
		fmt.Printf("%s  ────────────────────────────────────────────────────────────%s\n", aDim, aReset)

		if len(accounts) == 0 {
			fmt.Printf("  %sNo saved accounts yet. Press [a] to save current login.%s\n", aYellow, aReset)
		} else {
			for i, acc := range accounts {
				cursor := "  "
				nameCol := ""
				if i == selected {
					cursor = fmt.Sprintf("%s▸ %s", aCyan, aReset)
					nameCol = aBold + aCyan
				}
				badge := fmt.Sprintf("%s○%s", aDim, aReset)
				activeTag := ""
				if acc.Active {
					badge = fmt.Sprintf("%s●%s", aGreen, aReset)
					activeTag = fmt.Sprintf(" %s(active)%s", aGreen, aReset)
				}
				num := fmt.Sprintf("%s[%d]%s", aDim, i+1, aReset)
				fmt.Printf("  %s%s %s %s%-10s%s  %-28s%s\n",
					cursor, num, badge, nameCol, acc.Name, aReset, acc.Email, activeTag)
			}
		}

		fmt.Printf("%s  ────────────────────────────────────────────────────────────%s\n", aDim, aReset)
		fmt.Printf("  %s↑/↓/j/k%s move  ·  %sEnter / 1-9%s switch  ·  %ss%s swap  ·  %sr%s reload\n",
			aBold, aReset, aBold, aReset, aBold, aReset, aBold, aReset)
		fmt.Printf("  %sa%s save active login  ·  %sq / Esc%s close\n", aBold, aReset, aBold, aReset)

		if statusMsg != "" {
			fmt.Printf("\n  %s\n", statusMsg)
		}

		key, err := readSingleKey()
		if err != nil {
			return 0
		}

		switch key {
		case "q", "esc":
			return 0
		case "up", "k":
			if len(accounts) > 0 {
				selected = (selected - 1 + len(accounts)) % len(accounts)
			}
			statusMsg = ""
		case "down", "j":
			if len(accounts) > 0 {
				selected = (selected + 1) % len(accounts)
			}
			statusMsg = ""
		case "enter":
			if len(accounts) > 0 {
				return applyMenuSwitch(accounts[selected].Name, opts)
			}
		case "s":
			if len(accounts) >= 2 {
				info, rel, serr := Swap(true, opts)
				if serr != nil {
					statusMsg = fmt.Sprintf("%sError:%s %v", aRed, aReset, serr)
					continue
				}
				printPopupResult(info, rel)
				return 0
			}
			statusMsg = fmt.Sprintf("%sNeed at least 2 saved accounts to swap.%s", aYellow, aReset)
		case "r":
			rel, rerr := ReloadAgyPanes(opts)
			if rerr != nil {
				statusMsg = fmt.Sprintf("%sError:%s %v", aRed, aReset, rerr)
				continue
			}
			printPopupResult(nil, rel)
			return 0
		case "a":
			fmt.Print("\n  Save current wincred login as name (e.g. alt): ")
			rd := bufio.NewReader(os.Stdin)
			line, _ := rd.ReadString('\n')
			name := strings.TrimSpace(line)
			if name == "" {
				statusMsg = fmt.Sprintf("%sCancelled.%s", aDim, aReset)
				continue
			}
			info, serr := Save(name)
			if serr != nil {
				statusMsg = fmt.Sprintf("%sError:%s %v", aRed, aReset, serr)
				continue
			}
			statusMsg = fmt.Sprintf("%sSaved %q (%s)%s", aGreen, info.Name, info.Email, aReset)
		default:
			if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
				idx := int(key[0] - '1')
				if idx < len(accounts) {
					return applyMenuSwitch(accounts[idx].Name, opts)
				}
			}
		}
	}
}

func applyMenuSwitch(name string, opts ReloadOptions) int {
	fmt.Printf("\n  %sSwitching to %q and reloading idle agy panes...%s\n", aCyan, name, aReset)
	info, rel, err := Use(name, true, opts)
	if err != nil {
		fmt.Printf("  %sError:%s %v\n", aRed, aReset, err)
		time.Sleep(1800 * time.Millisecond)
		return 1
	}
	printPopupResult(info, rel)
	return 0
}

func printPopupResult(info *AccountInfo, rel *ReloadResult) {
	if info != nil {
		fmt.Printf("  %s✓ Active:%s %s (%s)\n", aGreen, aReset, info.Name, info.Email)
	}
	if rel != nil {
		if len(rel.Reloaded) > 0 {
			fmt.Printf("  %s✓ Reloaded:%s %s\n", aGreen, aReset, strings.Join(rel.Reloaded, ", "))
		}
		if len(rel.SkippedBusy) > 0 {
			fmt.Printf("  %s! Busy (not interrupted):%s %s\n", aYellow, aReset, strings.Join(rel.SkippedBusy, ", "))
		}
		if len(rel.Failed) > 0 {
			fmt.Printf("  %s✗ Failed:%s %s\n", aRed, aReset, strings.Join(rel.Failed, "; "))
		}
	}
	time.Sleep(1100 * time.Millisecond)
}
