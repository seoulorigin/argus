import { useEffect, useRef, useState } from "react"
import { pb } from "@/lib/api"

export type Health = "UP" | "WARN" | "DOWN" | "UNKNOWN"

export interface ServiceStatus {
	key: string
	ns: string
	name: string
	status: Health
	ready: number
	total: number
	pods: number
	reason?: string
	incidents?: string[]
	cpuMilli: number
	memBytes: number
	cpuLimitMilli?: number
	memLimitBytes?: number
	probeN: number
	p95Ms: number
	failPct: number
	probeError?: string
}

export interface NodeStatus {
	name: string
	status: Health
	cpu: number
	mem: number
	reason?: string
}

export interface Incident {
	id: string
	rule: string
	targetType: "service" | "pod" | "node"
	target: string
	severity: "WARNING" | "CRITICAL"
	open: boolean
	threshold: string
	observed: string
	peak?: number
	window: string
	startedAt: string
	recoveredAt?: string
	note?: string
	detail?: Record<string, unknown>
}

export interface Overview {
	at: string
	cluster?: string
	services: ServiceStatus[] | null
	nodes: NodeStatus[] | null
	open: Incident[] | null
	recent: Incident[] | null
}

export interface Point {
	t: number
	c: number
	m: number
	/** Service probe p95 in ms and failure %, absent without enough samples. */
	p?: number
	f?: number
}

/** Poll period of the hub's evaluation. */
export const REFRESH_MS = 5000

/** Polls `path` every 5 seconds. `error` is set while the hub cannot be reached. */
export function usePolled<T>(path: string | null, query?: Record<string, string>): { data: T | null; error: boolean } {
	const [data, setData] = useState<T | null>(null)
	const [error, setError] = useState(false)
	const queryKey = JSON.stringify(query ?? {})
	const seq = useRef(0)

	useEffect(() => {
		if (!path) {
			setData(null)
			return
		}
		const mine = ++seq.current
		let timer: ReturnType<typeof setTimeout>
		const tick = async () => {
			try {
				const next = await pb.send<T>(path, { query: JSON.parse(queryKey), requestKey: null })
				if (mine === seq.current) {
					setData(next)
					setError(false)
				}
			} catch {
				if (mine === seq.current) setError(true)
			}
			if (mine === seq.current) timer = setTimeout(tick, REFRESH_MS)
		}
		tick()
		return () => {
			seq.current++
			clearTimeout(timer)
		}
	}, [path, queryKey])

	return { data, error }
}

/** Seconds since `iso`, re-evaluated every second. */
export function useAgeSeconds(iso: string | undefined): number | null {
	const [, force] = useState(0)
	useEffect(() => {
		const id = setInterval(() => force((n) => n + 1), 1000)
		return () => clearInterval(id)
	}, [])
	if (!iso) return null
	return Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000))
}

export interface IncidentEvent {
	id: string
	at: string
	kind: "CRITICAL" | "WARNING" | "RECOVERED"
	incident: Incident
}

/** Turns incidents into the event stream operators read: one entry when it opened and one when it recovered. */
export function incidentEvents(incidents: Incident[]): IncidentEvent[] {
	const events: IncidentEvent[] = []
	for (const inc of incidents) {
		events.push({ id: `${inc.id}#open`, at: inc.startedAt, kind: inc.severity, incident: inc })
		if (!inc.open && inc.recoveredAt) {
			events.push({ id: `${inc.id}#rec`, at: inc.recoveredAt, kind: "RECOVERED", incident: inc })
		}
	}
	return events.sort((a, b) => b.at.localeCompare(a.at))
}

export function formatDuration(seconds: number): string {
	if (seconds < 60) return `${seconds}s`
	const m = Math.floor(seconds / 60)
	if (m < 60) return `${m}m ${seconds % 60}s`
	return `${Math.floor(m / 60)}h ${m % 60}m`
}
