// backfill-river-topology sequences rivers upstream→downstream from NHDPlus
// topology (web#392), replacing geographic proxies as the ordering basis.
//
// Ordering has leaned on put_in_elevation_ft DESC (mig 000150) with longitude
// as a fallback. Both are proxies for flow direction and each fails
// predictably: elevation on flat rivers (the Green through Canyonlands, the
// Menominee) where the real drop sits inside USGS EPQS noise, and longitude on
// any river not flowing west→east. NLDI's DM navigation returns flowlines in
// true downstream order, which is the actual answer rather than a correlate.
//
// Per river this costs TWO NLDI calls regardless of how many runs it has:
// UM from any member to find the headwater, then DM from there to sweep the
// whole stem. Verified on Clear Creek — seeding from the DOWNSTREAM-most run
// (Golden WWP, 2885178) still walks up to 2885304, the upstream-most run — so
// seed choice affects nothing but which flowline the UM leg starts at.
//
// The resulting order is cached in river_flowline_order, so adding a run or
// gauge later is a lookup, not another round trip.
//
// Gauges are the case that needs this most: DWR gauges have NO elevation at
// all (0 of 19 in prod), so gauge-only dashboard groups fall straight through
// to the longitude tier today. -gauges snaps watched gauges onto the network
// (filling gauges.comid, which exists but is 0/2166 populated) and assigns
// their sequence from the same cached orders, so runs and gauges interleave
// on one comparable key instead of two incomparable ones.
//
// Usage:
//
//	backfill-river-topology -dry-run            # list rivers that would sync, no writes, no NLDI
//	backfill-river-topology                     # sync rivers not yet synced
//	backfill-river-topology -force              # re-sync every eligible river
//	backfill-river-topology -gauges             # also snap + assign gauges
//	backfill-river-topology -river <uuid>       # one river only
//	backfill-river-topology -infer-only         # re-infer only; no NLDI at all
//
// Idempotent: a re-run with no -force reports rivers=0 once every river has a
// topology_synced_at. Reads DATABASE_URL from the environment.
//
// -infer-only is the cheap maintenance pass (web#406). Runs whose comid is not
// on the sequenced mainstem — forks, tributary runs, put-ins snapped across a
// confluence — get a sequence interpolated from their put-in elevation instead
// of staying NULL, because a NULL forces every list to blend two ordering keys
// and no pairwise comparator can do that transitively. Each run is placed on
// create, but a guess is only as good as the surveyed runs present at the time,
// so this recomputes every one against current topology. It touches no network.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/db"
	"github.com/h2oflow/h2oflow/apps/api/internal/rivertopology"
)

func main() {
	var (
		dryRun     = flag.Bool("dry-run", false, "report what would sync; no writes and no NLDI calls")
		force      = flag.Bool("force", false, "re-sync rivers that already have topology_synced_at")
		doGauges   = flag.Bool("gauges", false, "also resolve gauges.comid and assign gauge sequences")
		gaugeLimit = flag.Int("gauge-limit", 200, "max gauges to snap in one run")
		river      = flag.String("river", "", "sync only this river id")
		pauseMS    = flag.Int("pause-ms", 250, "delay between NLDI calls")
		inferOnly  = flag.Bool("infer-only", false, "re-infer sequences from cached topology; no NLDI")
	)
	flag.Parse()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	s := rivertopology.New(pool)
	pause := time.Duration(*pauseMS) * time.Millisecond

	if *inferOnly {
		n, err := s.InferAll(ctx)
		if err != nil {
			log.Fatalf("infer: %v", err)
		}
		fmt.Printf("inferred=%d\n", n)
		return
	}

	var rivers []string
	if *river != "" {
		rivers = []string{*river}
	} else {
		rivers, err = s.RiversNeedingSync(ctx, *force)
		if err != nil {
			log.Fatalf("list rivers: %v", err)
		}
	}

	if *dryRun {
		fmt.Printf("dry-run: %d river(s) would sync (force=%v)\n", len(rivers), *force)
		for _, id := range rivers {
			var name string
			var runs int
			_ = pool.QueryRow(ctx, `
				SELECT rv.name, count(*)
				FROM rivers rv
				JOIN user_reaches ur ON ur.river_id = rv.id AND ur.deleted_at IS NULL
				WHERE rv.id = $1
				GROUP BY rv.name
			`, id).Scan(&name, &runs)
			fmt.Printf("  %s  %-40s %d run(s)\n", id, name, runs)
		}
		return
	}

	var synced, failed, uncovered, inferred int
	for _, id := range rivers {
		res := s.SyncRiver(ctx, id)
		switch {
		case res.Err != nil:
			failed++
			log.Printf("FAIL %s (%s): %v", id, res.RiverName, res.Err)
		case res.Members < 2:
			log.Printf("skip %s (%s): %d member comid(s), nothing to order", id, res.RiverName, res.Members)
		default:
			synced++
			uncovered += len(res.Uncovered)
			inferred += res.Inferred
			// sequenced counts SURVEYED runs only and inferred counts guesses,
			// so the two never overlap — a river reading sequenced=8 inferred=2
			// has 10 ordered runs, 2 of them placed by elevation.
			log.Printf("ok   %-30s members=%d flowlines=%d sequenced=%d inferred=%d headwater=%s uncovered=%v",
				res.RiverName, res.Members, res.Flowlines, res.Sequenced, res.Inferred, res.Headwater, res.Uncovered)
		}
		time.Sleep(pause)
	}
	fmt.Printf("rivers: synced=%d failed=%d uncovered_members=%d inferred_runs=%d\n", synced, failed, uncovered, inferred)

	if *doGauges {
		n, err := s.ResolveGaugeComIDs(ctx, *gaugeLimit, pause)
		if err != nil {
			log.Fatalf("resolve gauge comids: %v", err)
		}
		fmt.Printf("gauges: comid resolved=%d\n", n)

		m, err := s.AssignGauges(ctx)
		if err != nil {
			log.Fatalf("assign gauges: %v", err)
		}
		fmt.Printf("gauges: sequence assigned=%d\n", m)
	}
}
