import { memo } from "react"
import InfoBar from "./system/info-bar"
import { useSystemData } from "./system/use-system-data"
import { CpuChart, ContainerCpuChart } from "./system/charts/cpu-charts"
import { MemoryChart, ContainerMemoryChart, SwapChart } from "./system/charts/memory-charts"
import { BandwidthChart, ContainerNetworkChart } from "./system/charts/network-charts"
import { LazyNetworkMonitorsTable } from "./system/lazy-tables"
import { LoadAverageChart } from "./system/charts/load-average-chart"
import { supportsNetworkMonitors } from "@/lib/utils"

export default memo(function SystemDetail({ id }: { id: string }) {
	const systemData = useSystemData(id)
	const {
		system,
		systemStats,
		containerData,
		chartData,
		containerChartConfigs,
		details,
		grid,
		setGrid,
		displayMode,
		setDisplayMode,
		maxValues,
		isLongerChart,
		showMax,
		dataEmpty,
		isPodman,
	} = systemData

	if (!system.id) {
		return null
	}

	const hasContainers = containerData.length > 0
	const hasNetworkMonitors = supportsNetworkMonitors(system)
	const coreProps = { chartData, grid, dataEmpty, showMax, isLongerChart, maxValues }

	return (
		<div className="grid gap-6 overflow-x-clip">
			<InfoBar
				system={system}
				chartData={chartData}
				grid={grid}
				setGrid={setGrid}
				displayMode={displayMode}
				setDisplayMode={setDisplayMode}
				details={details}
			/>
			<div className="grid xl:grid-cols-2 gap-6">
				<CpuChart {...coreProps} />
				{hasContainers && (
					<ContainerCpuChart
						chartData={chartData}
						grid={grid}
						dataEmpty={dataEmpty}
						isPodman={isPodman}
						cpuConfig={containerChartConfigs.cpu}
					/>
				)}
				<MemoryChart {...coreProps} />
				{hasContainers && (
					<ContainerMemoryChart
						chartData={chartData}
						grid={grid}
						dataEmpty={dataEmpty}
						isPodman={isPodman}
						memoryConfig={containerChartConfigs.memory}
					/>
				)}
				<BandwidthChart {...coreProps} systemStats={systemStats} />
				{hasContainers && (
					<ContainerNetworkChart
						chartData={chartData}
						grid={grid}
						dataEmpty={dataEmpty}
						isPodman={isPodman}
						networkConfig={containerChartConfigs.network}
					/>
				)}
				<SwapChart chartData={chartData} grid={grid} dataEmpty={dataEmpty} systemStats={systemStats} />
				<LoadAverageChart chartData={chartData} grid={grid} dataEmpty={dataEmpty} />
			</div>
			{hasNetworkMonitors && <LazyNetworkMonitorsTable systemId={system.id} />}
		</div>
	)
})
