package replication

import (
	"strconv"
	"strings"
)

// MySQL renamed the whole replication vocabulary in 8.0.22: CHANGE MASTER TO
// became CHANGE REPLICATION SOURCE TO, START SLAVE became START REPLICA, and
// every MASTER_ option became a SOURCE_ one. The old words still work in
// 8.0 but are gone in 9.0, and the new ones do not exist in 5.7.
//
// gora speaks whichever the server understands. Telling somebody their
// perfectly good 5.7 replica is unsupported because of vocabulary would be
// a poor reason.
type dialect struct {
	modern bool
}

func dialectFor(version string) dialect {
	return dialect{modern: atLeast(version, 8, 0, 22)}
}

// atLeast compares a MySQL version string ("8.0.36-log", "5.7.44") against a
// minimum.
func atLeast(version string, major, minor, patch int) bool {
	nums := parseVersion(version)
	if len(nums) < 3 {
		return false
	}
	switch {
	case nums[0] != major:
		return nums[0] > major
	case nums[1] != minor:
		return nums[1] > minor
	default:
		return nums[2] >= patch
	}
}

func parseVersion(version string) []int {
	// Everything after the first dash is a build suffix.
	if i := strings.IndexAny(version, "-+ "); i >= 0 {
		version = version[:i]
	}
	parts := strings.Split(version, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		nums = append(nums, n)
	}
	return nums
}

func (d dialect) changeSource() string {
	if d.modern {
		return "CHANGE REPLICATION SOURCE TO"
	}
	return "CHANGE MASTER TO"
}

func (d dialect) startReplica() string {
	if d.modern {
		return "START REPLICA"
	}
	return "START SLAVE"
}

func (d dialect) stopReplica() string {
	if d.modern {
		return "STOP REPLICA"
	}
	return "STOP SLAVE"
}

func (d dialect) resetReplica() string {
	if d.modern {
		return "RESET REPLICA ALL"
	}
	return "RESET SLAVE ALL"
}

func (d dialect) showReplicaStatus() string {
	if d.modern {
		return "SHOW REPLICA STATUS"
	}
	return "SHOW SLAVE STATUS"
}

// opt names one of the CHANGE ... TO options.
func (d dialect) opt(name string) string {
	if d.modern {
		return "SOURCE_" + name
	}
	return "MASTER_" + name
}

// statusField names a column of the replica status, which was renamed with
// everything else.
func (d dialect) statusField(modern, legacy string) string {
	if d.modern {
		return modern
	}
	return legacy
}
