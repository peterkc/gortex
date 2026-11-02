// Package fixture is a bounded callgraph integration target, not study evidence.
package fixture

func Seam() int { return 1 }

func Direct() int { return Seam() }

func Second() int { return Direct() }

func Third() int { return Second() }

func Fourth() int { return Third() }
