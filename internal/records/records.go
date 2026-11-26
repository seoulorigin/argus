// Package records handles creating longer records and deleting old records.
package records

import (
	"encoding/json"
	"math"
	"time"

	"argus/internal/entities/container"
	"argus/internal/entities/system"

	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"
)

type RecordManager struct {
	app core.App
}

type LongerRecordData struct {
	shorterType        string
	longerType         string
	longerTimeDuration time.Duration
	minShorterRecords  int
}

// shorterRecordCutoff returns the time after which shorter records are included in a longer record.
//
// 1m records are samples created independently of this job, so the window is the full longer period.
// Other types are created by this job, a few ms after it starts. The record created one full period
// ago covers the previous period but lands right on the window boundary. Records created by this job
// are always a multiple of the job interval (10 minutes) apart, so moving the cutoff forward by half
// that interval excludes it without ever landing on another record.
func (d *LongerRecordData) shorterRecordCutoff(now time.Time) time.Time {
	cutoff := now.Add(d.longerTimeDuration)
	if d.shorterType != "1m" {
		cutoff = cutoff.Add(5 * time.Minute)
	}
	return cutoff
}

type RecordIds []struct {
	Id string `db:"id"`
}

func NewRecordManager(app core.App) *RecordManager {
	return &RecordManager{app}
}

// StatsRecord holds the raw stats blob of a shorter record.
type StatsRecord struct {
	Stats []byte `db:"stats"`
}

// Create longer records by averaging shorter records
func (rm *RecordManager) CreateLongerRecords() {
	now := time.Now().UTC()
	longerRecordData := []LongerRecordData{
		{
			shorterType: "1m",
			// change to 9 from 10 to allow edge case timing or short pauses
			minShorterRecords:  9,
			longerType:         "10m",
			longerTimeDuration: -10 * time.Minute,
		},
		{
			shorterType:        "10m",
			minShorterRecords:  2,
			longerType:         "20m",
			longerTimeDuration: -20 * time.Minute,
		},
		{
			shorterType:        "20m",
			minShorterRecords:  6,
			longerType:         "120m",
			longerTimeDuration: -120 * time.Minute,
		},
		{
			shorterType:        "120m",
			minShorterRecords:  4,
			longerType:         "480m",
			longerTimeDuration: -480 * time.Minute,
		},
	}
	// wrap the operations in a transaction
	// Pocketbase cron does not handle errors, log them here.
	err := rm.app.RunInTransaction(func(txApp core.App) error {
		var err error

		collections := [2]*core.Collection{}
		collections[0], err = txApp.FindCachedCollectionByNameOrId("system_stats")
		if err != nil {
			return err
		}
		collections[1], err = txApp.FindCachedCollectionByNameOrId("container_stats")
		if err != nil {
			return err
		}
		var systems RecordIds
		db := txApp.DB()

		if err := db.NewQuery("SELECT id FROM systems WHERE status='up'").All(&systems); err != nil {
			return err
		}

		// loop through all active systems, time periods, and collections
		for _, system := range systems {
			// log.Println("processing system", system.GetString("name"))
			for i := range longerRecordData {
				recordData := longerRecordData[i]
				// log.Println("processing longer record type", recordData.longerType)
				// add one minute padding for longer records because they are created slightly later than the job start time
				longerRecordPeriod := now.Add(recordData.longerTimeDuration + time.Minute)
				shorterRecordPeriod := recordData.shorterRecordCutoff(now)
				for _, collection := range collections {
					// check creation time of last longer record if not 10m, since 10m is created every run
					if recordData.longerType != "10m" {
						count, err := txApp.CountRecords(collection.Id, dbx.NewExp(
							"system = {:system} AND type = {:type} AND created > {:created}",
							dbx.Params{
								"type":    recordData.longerType,
								"system":  system.Id,
								"created": longerRecordPeriod.Format(types.DefaultDateLayout),
							},
						))
						if err != nil {
							return err
						}
						// continue if longer record exists
						if count > 0 {
							continue
						}
					}
					// get shorter records from the past x minutes, oldest first
					// since averaging keeps the last value of some fields
					var shorterRecords []StatsRecord

					params := dbx.Params{
						"type":    recordData.shorterType,
						"system":  system.Id,
						"created": shorterRecordPeriod.Format(types.DefaultDateLayout),
					}

					err := db.
						Select("stats").
						From(collection.Name).
						Where(dbx.NewExp(
							"system={:system} AND type={:type} AND created > {:created}",
							params,
						)).
						OrderBy("created").
						All(&shorterRecords)
					if err != nil {
						rm.app.Logger().Error("failed to fetch shorter records", "collection", collection.Name, "system", system.Id, "err", err)
						continue
					}

					// continue if not enough shorter records
					if len(shorterRecords) < recordData.minShorterRecords {
						continue
					}
					// average the shorter records and create longer record
					longerRecord := core.NewRecord(collection)
					longerRecord.Set("system", system.Id)
					longerRecord.Set("type", recordData.longerType)
					switch collection.Name {
					case "system_stats":
						longerRecord.Set("stats", AverageSystemStats(shorterRecords))
					case "container_stats":
						longerRecord.Set("stats", AverageContainerStats(shorterRecords))
					}
					if err := txApp.SaveNoValidate(longerRecord); err != nil {
						txApp.Logger().Error("failed to save longer record", "err", err)
					}
				}
			}
		}

		return nil
	})
	if err != nil {
		rm.app.Logger().Error("failed to create longer records", "err", err)
	}
}

func getCreatedTimeField(collectionName string, period time.Time) any {
	return period.Format(types.DefaultDateLayout)
}

// AverageSystemStats calculates the average stats of system_stats records.
// Records must be in creation order: some fields keep the value of the last record.
func AverageSystemStats(records []StatsRecord) system.Stats {
	stats := make([]system.Stats, 0, len(records))
	for _, record := range records {
		var s system.Stats
		if err := json.Unmarshal(record.Stats, &s); err != nil {
			continue
		}
		stats = append(stats, s)
	}
	return AverageSystemStatsSlice(stats)
}

// AverageSystemStatsSlice computes the average of a slice of system stats.
func AverageSystemStatsSlice(records []system.Stats) system.Stats {
	var sum system.Stats
	count := float64(len(records))
	if count == 0 {
		return sum
	}
	var cpuCoresSums []uint64
	var cpuBreakdownSums []float64

	for i := range records {
		stats := &records[i]
		sum.Cpu += stats.Cpu
		if stats.CpuBreakdown != nil {
			if len(cpuBreakdownSums) < len(stats.CpuBreakdown) {
				cpuBreakdownSums = append(cpuBreakdownSums, make([]float64, len(stats.CpuBreakdown)-len(cpuBreakdownSums))...)
			}
			for j, v := range stats.CpuBreakdown {
				cpuBreakdownSums[j] += v
			}
		}
		if stats.CpuCoresUsage != nil {
			if len(cpuCoresSums) < len(stats.CpuCoresUsage) {
				cpuCoresSums = append(cpuCoresSums, make([]uint64, len(stats.CpuCoresUsage)-len(cpuCoresSums))...)
			}
			for j, v := range stats.CpuCoresUsage {
				cpuCoresSums[j] += uint64(v)
			}
		}
		for j, v := range stats.MemBytes {
			sum.MemBytes[j] += v
		}
		sum.SwapBytes[0] += stats.SwapBytes[0]
		sum.SwapBytes[1] += stats.SwapBytes[1]
		sum.MemPct += stats.MemPct
		sum.NetworkSent += stats.NetworkSent
		sum.NetworkRecv += stats.NetworkRecv
		for j := range sum.LoadAvg {
			sum.LoadAvg[j] += stats.LoadAvg[j]
		}
		sum.Bandwidth[0] += stats.Bandwidth[0]
		sum.Bandwidth[1] += stats.Bandwidth[1]

		// peak values
		sum.MaxCpu = max(sum.MaxCpu, stats.MaxCpu, stats.Cpu)
		sum.MaxMemBytes = max(sum.MaxMemBytes, stats.MaxMemBytes, stats.MemBytes[1])
		sum.MaxNetworkSent = max(sum.MaxNetworkSent, stats.MaxNetworkSent, stats.NetworkSent)
		sum.MaxNetworkRecv = max(sum.MaxNetworkRecv, stats.MaxNetworkRecv, stats.NetworkRecv)
		sum.MaxBandwidth[0] = max(sum.MaxBandwidth[0], stats.MaxBandwidth[0], stats.Bandwidth[0])
		sum.MaxBandwidth[1] = max(sum.MaxBandwidth[1], stats.MaxBandwidth[1], stats.Bandwidth[1])

		if sum.NetworkInterfaces == nil {
			sum.NetworkInterfaces = make(map[string][4]uint64, len(stats.NetworkInterfaces))
		}
		for key, value := range stats.NetworkInterfaces {
			sum.NetworkInterfaces[key] = [4]uint64{
				sum.NetworkInterfaces[key][0] + value[0],
				sum.NetworkInterfaces[key][1] + value[1],
				max(sum.NetworkInterfaces[key][2], value[2]),
				max(sum.NetworkInterfaces[key][3], value[3]),
			}
		}
	}

	sum.Cpu = twoDecimals(sum.Cpu / count)
	for i := range sum.MemBytes {
		sum.MemBytes[i] = sum.MemBytes[i] / uint64(count)
	}
	sum.MemPct = twoDecimals(sum.MemPct / count)
	sum.SwapBytes[0] = sum.SwapBytes[0] / uint64(count)
	sum.SwapBytes[1] = sum.SwapBytes[1] / uint64(count)
	sum.NetworkSent = twoDecimals(sum.NetworkSent / count)
	sum.NetworkRecv = twoDecimals(sum.NetworkRecv / count)
	for i := range sum.LoadAvg {
		sum.LoadAvg[i] = twoDecimals(sum.LoadAvg[i] / count)
	}
	sum.Bandwidth[0] = sum.Bandwidth[0] / uint64(count)
	sum.Bandwidth[1] = sum.Bandwidth[1] / uint64(count)
	for key, value := range sum.NetworkInterfaces {
		sum.NetworkInterfaces[key] = [4]uint64{value[0] / uint64(count), value[1] / uint64(count), value[2], value[3]}
	}
	if len(cpuCoresSums) > 0 {
		avg := make(system.Uint8Slice, len(cpuCoresSums))
		for i := range cpuCoresSums {
			avg[i] = uint8(math.Round(float64(cpuCoresSums[i]) / count))
		}
		sum.CpuCoresUsage = avg
	}
	if len(cpuBreakdownSums) > 0 {
		avg := make([]float64, len(cpuBreakdownSums))
		for i := range cpuBreakdownSums {
			avg[i] = twoDecimals(cpuBreakdownSums[i] / count)
		}
		sum.CpuBreakdown = avg
	}
	return sum
}

// AverageContainerStats calculates the average stats of container_stats records.
// Records must be in creation order, matching the averaging of system stats.
func AverageContainerStats(records []StatsRecord) []container.Stats {
	allStats := make([][]container.Stats, 0, len(records))
	for _, record := range records {
		var cs []container.Stats
		if err := json.Unmarshal(record.Stats, &cs); err != nil {
			return []container.Stats{}
		}
		allStats = append(allStats, cs)
	}
	return AverageContainerStatsSlice(allStats)
}

// AverageContainerStatsSlice computes the average of container stats across multiple time periods.
func AverageContainerStatsSlice(records [][]container.Stats) []container.Stats {
	if len(records) == 0 {
		return []container.Stats{}
	}
	sums := make(map[string]*container.Stats)
	count := float64(len(records))

	for _, containerStats := range records {
		for i := range containerStats {
			stat := &containerStats[i]
			if _, ok := sums[stat.Name]; !ok {
				sums[stat.Name] = &container.Stats{Name: stat.Name}
			}
			sums[stat.Name].Cpu += stat.Cpu
			sums[stat.Name].Mem += stat.Mem
			sentBytes := stat.Bandwidth[0]
			recvBytes := stat.Bandwidth[1]
			if sentBytes == 0 && recvBytes == 0 && (stat.NetworkSent != 0 || stat.NetworkRecv != 0) {
				sentBytes = uint64(stat.NetworkSent * 1024 * 1024)
				recvBytes = uint64(stat.NetworkRecv * 1024 * 1024)
			}
			sums[stat.Name].Bandwidth[0] += sentBytes
			sums[stat.Name].Bandwidth[1] += recvBytes
		}
	}

	result := make([]container.Stats, 0, len(sums))
	for _, value := range sums {
		result = append(result, container.Stats{
			Name:      value.Name,
			Cpu:       twoDecimals(value.Cpu / count),
			Mem:       twoDecimals(value.Mem / count),
			Bandwidth: [2]uint64{uint64(float64(value.Bandwidth[0]) / count), uint64(float64(value.Bandwidth[1]) / count)},
		})
	}
	return result
}

/* Round float to two decimals */

func twoDecimals(value float64) float64 {
	return math.Round(value*100) / 100
}
