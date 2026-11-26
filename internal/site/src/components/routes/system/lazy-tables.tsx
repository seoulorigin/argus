import { lazy } from "react"
import { useIntersectionObserver } from "@/lib/use-intersection-observer"
import { cn } from "@/lib/utils"
import { useNetworkMonitors } from "@/lib/use-network-monitors"

const NetworkMonitorsTable = lazy(() => import("../../network-monitors-table/network-monitors-table"))

export function LazyNetworkMonitorsTable({ systemId }: { systemId: string }) {
	const { isIntersecting, ref } = useIntersectionObserver({ rootMargin: "90px" })
	return (
		<div ref={ref} className={cn(isIntersecting && "contents")}>
			{isIntersecting && <SystemNetworkMonitorsTable systemId={systemId} />}
		</div>
	)
}

function SystemNetworkMonitorsTable({ systemId }: { systemId: string }) {
	const { monitors, isLoading } = useNetworkMonitors({ systemId })
	return <NetworkMonitorsTable systemId={systemId} monitors={monitors} isLoading={isLoading} />
}
