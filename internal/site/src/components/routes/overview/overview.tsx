import { useLingui } from "@lingui/react/macro"
import { getPagePath } from "@nanostores/router"
import { useStore } from "@nanostores/react"
import { memo, useEffect, useMemo, useState } from "react"
import { $router, Link } from "@/components/router"
import { $allSystemsByName } from "@/lib/stores"
import { cn } from "@/lib/utils"
import { Card } from "@/components/ui/card"
import { ArrowUpRightIcon } from "lucide-react"
import {
	formatDuration,
	type Health,
	type Incident,
	incidentEvents,
	type NodeStatus,
	type Overview,
	type Point,
	type ServiceStatus,
	useAgeSeconds,
	usePolled,
} from "@/lib/overview"
import TrendChart from "./spark-chart"

/** One colour per state. The names resolve to the design tokens in index.css, so light and dark share them. */
const STATE = {
	UP: { dot: "bg-up", text: "text-up", css: "var(--up)" },
	WARN: { dot: "bg-warn", text: "text-warn", css: "var(--warn)" },
	DOWN: { dot: "bg-down", text: "text-down", css: "var(--down)" },
	UNKNOWN: { dot: "bg-muted-foreground/50", text: "text-muted-foreground", css: "var(--muted-foreground)" },
} as const

const SEVERITY = {
	CRITICAL: { text: "text-down" },
	WARNING: { text: "text-warn" },
	RECOVERED: { text: "text-up" },
} as const

const SYSTEM_NAMESPACES = /^(kube-|calico-|tigera-|argus)/

const mib = (bytes: number) => `${Math.round(bytes / 1048576)} MiB`
const ms = (v: number) => `${v < 10 ? v.toFixed(1) : Math.round(v)} ms`

type MetricKey = "p95" | "cpu" | "mem" | "fail"

function useSelection(services: ServiceStatus[], nodes: NodeStatus[]) {
	const [selected, setSelected] = useState<string | null>(null)
	// until the operator picks something, follow the most urgent target
	const auto = useMemo(() => {
		const bad = services.find((s) => s.status === "DOWN") ?? services.find((s) => s.status === "WARN")
		if (bad) return `service/${bad.key}`
		if (services[0]) return `service/${services[0].key}`
		return nodes[0] ? `node/${nodes[0].name}` : null
	}, [services, nodes])
	return [selected ?? auto, setSelected, selected === null] as const
}

export default memo(function OverviewPage() {
	const { t } = useLingui()
	const { data, error } = usePolled<Overview>("/api/argus/status")
	const [showSystem, setShowSystem] = useState(false)
	const age = useAgeSeconds(data?.at)

	useEffect(() => {
		document.title = `${t`Monitoring Overview`} / Argus`
	}, [t])

	const allServices = data?.services ?? []
	const nodes = data?.nodes ?? []
	const services = useMemo(
		() => (showSystem ? allServices : allServices.filter((s) => !SYSTEM_NAMESPACES.test(s.ns))),
		[allServices, showSystem]
	)
	const counts = useMemo(() => {
		const c = { UP: 0, WARN: 0, DOWN: 0, UNKNOWN: 0 }
		for (const s of services) c[s.status]++
		return c
	}, [services])
	const events = useMemo(
		() => incidentEvents([...(data?.open ?? []), ...(data?.recent ?? [])]).slice(0, 14),
		[data]
	)
	const [selected, setSelected] = useSelection(services, nodes)
	const stale = error || (age !== null && age > 15)

	if (!data) {
		return (
			<div className="py-24 text-center text-muted-foreground">
				{error ? t`Cannot reach the hub` : t`Loading…`}
			</div>
		)
	}

	const nodesDown = nodes.filter((n) => n.status === "DOWN").length
	const ordered = [...services].sort((a, b) => RANK[a.status] - RANK[b.status])

	return (
		<div className="grid grid-cols-1 gap-6">
			<div className="flex flex-wrap items-end justify-between gap-4">
				<div>
					<h1 className="text-[1.75rem] leading-8 font-medium tracking-[-0.01em]">{t`Monitoring Overview`}</h1>
					<p className="mt-1 font-mono text-xs text-muted-foreground">
						{data.cluster ? `${data.cluster} · ` : ""}
						{nodes.length} {t`nodes`} · {services.length} {t`services`}
					</p>
				</div>
				<div className={cn("flex items-center gap-2 font-mono text-xs tabular-nums", stale ? "text-down" : "text-muted-foreground")}>
					<span
						className={cn(
							"size-2 rounded-full",
							stale ? "bg-down shadow-[0_0_0_3px_color-mix(in_srgb,var(--down)_20%,transparent)]" : "bg-up shadow-[0_0_0_3px_color-mix(in_srgb,var(--up)_20%,transparent)]"
						)}
					/>
					{error ? t`Connection lost` : age === null ? "" : t`Updated ${age} sec ago`}
				</div>
			</div>

			<section className="grid gap-3 sm:grid-cols-2 xl:grid-cols-[repeat(2,10rem)_minmax(0,1fr)]" aria-label={t`Summary`}>
				<Stat label={t`Nodes`} value={`${nodes.length - nodesDown}/${nodes.length}`} note={nodesDown ? `${nodesDown} ${t`not ready`}` : t`all ready`} />
				<Stat label={t`Services`} value={services.length} note={showSystem ? t`all namespaces` : t`user namespaces`} />
				<Card className="px-5 py-4 grid gap-3 content-between sm:col-span-2 xl:col-span-1">
					<div className="flex flex-wrap gap-x-10 gap-y-2">
						<Count label={t`Healthy`} value={counts.UP} state="UP" />
						<Count label={t`Warning`} value={counts.WARN} state="WARN" />
						<Count label={t`Down`} value={counts.DOWN} state="DOWN" />
					</div>
					<div className="flex h-2 gap-0.5 overflow-hidden rounded-sm bg-muted" role="img" aria-label={t`Share of services by status`}>
						{(["UP", "WARN", "DOWN"] as const).map(
							(k) => counts[k] > 0 && <i key={k} className="block min-w-1 transition-[flex-grow] duration-300" style={{ flexGrow: counts[k], background: STATE[k].css }} />
						)}
					</div>
				</Card>
			</section>

			<div className="grid grid-cols-1 gap-6 xl:grid-cols-[minmax(0,1fr)_400px] items-start">
				<div className="grid gap-6 min-w-0">
					<Panel
						title={t`Services`}
						right={
							<label className="flex cursor-pointer select-none items-center gap-2 text-sm text-muted-foreground">
								<input type="checkbox" className="size-3.5 accent-brand" checked={showSystem} onChange={(e) => setShowSystem(e.target.checked)} />
								{t`Show system namespaces`}
							</label>
						}
					>
						{services.length === 0 ? (
							<p className="py-6 text-center text-sm text-muted-foreground">
								{allServices.length === 0 ? t`No Kubernetes services found yet` : t`No services in the selected namespaces`}
							</p>
						) : (
							<div className="grid max-h-[30rem] grid-cols-[repeat(auto-fill,minmax(220px,1fr))] gap-2 overflow-y-auto pe-1">
								{ordered.map((s) => (
									<ServiceTile key={s.key} s={s} active={selected === `service/${s.key}`} onSelect={() => setSelected(`service/${s.key}`)} />
								))}
							</div>
						)}
						{nodes.length > 0 && (
							<div className="mt-4 flex flex-wrap gap-2 border-t pt-4">
								{nodes.map((n) => (
									<NodeChip key={n.name} n={n} active={selected === `node/${n.name}`} onSelect={() => setSelected(`node/${n.name}`)} />
								))}
							</div>
						)}
					</Panel>

					<TrendPanel selected={selected} services={allServices} nodes={nodes} />
				</div>

				<Panel title={t`Recent incidents`} right={<span className="font-mono text-xs text-muted-foreground">{data.open?.length ?? 0} {t`open`}</span>}>
					<IncidentList events={events} />
				</Panel>
			</div>
		</div>
	)
})

const RANK = { DOWN: 0, WARN: 1, UNKNOWN: 2, UP: 3 } as const

function Stat({ label, value, note }: { label: string; value: string | number; note: string }) {
	return (
		<Card className="grid content-start gap-1 px-5 py-4">
			<span className="text-sm text-muted-foreground">{label}</span>
			<span className="text-[2rem] leading-9 font-medium tracking-[-0.01em] tabular-nums">{value}</span>
			<span className="font-mono text-xs text-muted-foreground">{note}</span>
		</Card>
	)
}

function Count({ label, value, state }: { label: string; value: number; state: Health }) {
	return (
		<div className={cn("flex items-baseline gap-2", value === 0 && "opacity-60")}>
			<span className={cn("size-2 self-center rounded-full", STATE[state].dot)} />
			<span className="text-[2rem] leading-9 font-medium tracking-[-0.01em] tabular-nums">{value}</span>
			<span className="text-sm text-muted-foreground">{label}</span>
		</div>
	)
}

function Panel({ title, right, children }: { title: string; right?: React.ReactNode; children: React.ReactNode }) {
	return (
		<Card className="min-w-0 p-5">
			<header className="mb-4 flex min-h-8 flex-wrap items-center justify-between gap-4">
				<h2 className="text-[0.9375rem] leading-5 font-medium">{title}</h2>
				{right}
			</header>
			{children}
		</Card>
	)
}

function resourceText(s: ServiceStatus) {
	const cpu = s.cpuLimitMilli ? `${Math.round((s.cpuMilli / s.cpuLimitMilli) * 100)}%` : `${Math.round(s.cpuMilli)}m`
	const mem = s.memLimitBytes ? `${Math.round((s.memBytes / s.memLimitBytes) * 100)}%` : mib(s.memBytes)
	return { cpu, mem }
}

/** "p95 24 ms", "p95 286 ms · 17% err", or the dominant failure class ("TIMEOUT") when most probes fail. */
function probeText(s: ServiceStatus, none: string): { text: string; bad: boolean } {
	if (!s.probeN) return { text: none, bad: false }
	if (s.failPct > 50) return { text: (s.probeError ?? "FAILED").toUpperCase(), bad: true }
	const rtt = s.p95Ms > 0 ? `p95 ${ms(s.p95Ms)}` : "–"
	return s.failPct > 0 ? { text: `${rtt} · ${Math.round(s.failPct)}% err`, bad: s.failPct > 10 } : { text: rtt, bad: false }
}

function ServiceTile({ s, active, onSelect }: { s: ServiceStatus; active: boolean; onSelect: () => void }) {
	const { t } = useLingui()
	const c = STATE[s.status]
	const r = resourceText(s)
	const probe = probeText(s, t`no probe data`)
	return (
		<button
			type="button"
			onClick={onSelect}
			aria-pressed={active}
			title={s.reason || undefined}
			className={cn(
				"grid gap-1 rounded-xl border bg-background p-3 text-start transition-colors hover:bg-accent",
				active && "border-brand bg-accent shadow-[inset_0_1px_0_rgb(255_255_255/0.06)]"
			)}
		>
			<span className="flex min-w-0 items-center gap-2">
				<span className={cn("size-2 shrink-0 rounded-full", c.dot)} />
				<span className="min-w-0 flex-1 truncate font-medium">{s.name}</span>
				<span className={cn("text-xs", c.text)}>{s.status}</span>
			</span>
			<span className="flex justify-between gap-2 font-mono text-xs tabular-nums">
				<span className={cn("truncate", probe.bad ? "text-down" : "text-muted-foreground")}>{probe.text}</span>
				{s.status !== "UNKNOWN" && (
					<span className="shrink-0 text-muted-foreground">
						{s.ready}/{s.total} {t`ready`}
					</span>
				)}
			</span>
			<span className="flex justify-between gap-2 font-mono text-xs tabular-nums text-muted-foreground">
				<span>CPU {r.cpu}</span>
				<span>MEM {r.mem}</span>
			</span>
		</button>
	)
}

function NodeChip({ n, active, onSelect }: { n: NodeStatus; active: boolean; onSelect: () => void }) {
	const c = STATE[n.status]
	const system = useStore($allSystemsByName)[n.name]
	return (
		<div className={cn("flex h-8 items-center gap-2 rounded-md border px-3 text-sm transition-colors hover:bg-accent", active && "border-brand bg-accent")}>
			<span className={cn("size-2 rounded-full", c.dot)} />
			<button type="button" aria-pressed={active} onClick={onSelect} className="font-medium" title={n.reason || undefined}>
				{n.name}
			</button>
			<span className="font-mono text-xs tabular-nums text-muted-foreground">
				CPU {Math.round(n.cpu)}% · MEM {Math.round(n.mem)}%
			</span>
			{system && (
				<Link href={getPagePath($router, "system", { id: system.id })} className="text-muted-foreground hover:text-foreground" aria-label={n.name}>
					<ArrowUpRightIcon className="size-3.5" />
				</Link>
			)}
		</div>
	)
}

function TrendPanel({ selected, services, nodes }: { selected: string | null; services: ServiceStatus[]; nodes: NodeStatus[] }) {
	const { t } = useLingui()
	const { data: points } = usePolled<Point[]>(selected ? "/api/argus/history" : null, selected ? { key: selected } : undefined)
	const series = points ?? []
	const [metric, setMetric] = useState<MetricKey>("p95")

	const service = selected?.startsWith("service/") ? services.find((s) => `service/${s.key}` === selected) : undefined
	const node = selected?.startsWith("node/") ? nodes.find((n) => `node/${n.name}` === selected) : undefined
	const label = service ? service.key : node ? node.name : ""

	const tabs: { key: MetricKey; label: string }[] = service
		? [
				{ key: "p95", label: t`Response p95` },
				{ key: "cpu", label: "CPU" },
				{ key: "mem", label: t`Memory` },
				{ key: "fail", label: t`Failed probes` },
			]
		: [
				{ key: "cpu", label: "CPU" },
				{ key: "mem", label: t`Memory` },
			]
	const active: MetricKey = tabs.some((x) => x.key === metric) ? metric : "cpu"

	const pct = (v: number) => `${Math.round(v)}%`
	let cfg: Pick<React.ComponentProps<typeof TrendChart>, "pick" | "max" | "threshold" | "thresholdLabel" | "format" | "tick">
	if (active === "p95") {
		cfg = { pick: (p) => p.p, threshold: 250, thresholdLabel: "250 ms", format: ms }
	} else if (active === "fail") {
		cfg = { pick: (p) => (p.p !== undefined || p.f !== undefined ? (p.f ?? 0) : undefined), max: 100, threshold: 10, thresholdLabel: "10%", format: pct, tick: pct }
	} else if (service && active === "cpu" && service.cpuLimitMilli) {
		const lim = service.cpuLimitMilli
		cfg = { pick: (p) => (p.c / lim) * 100, max: 100, threshold: 85, thresholdLabel: t`85% of limit`, format: pct, tick: pct }
	} else if (service && active === "mem" && service.memLimitBytes) {
		const lim = service.memLimitBytes
		cfg = { pick: (p) => (p.m / lim) * 100, max: 100, threshold: 90, thresholdLabel: t`90% of limit`, format: pct, tick: pct }
	} else if (service) {
		cfg = active === "cpu" ? { pick: (p) => p.c, format: (v) => `${Math.round(v)}m` } : { pick: (p) => p.m, format: mib }
	} else {
		cfg = { pick: (p) => (active === "cpu" ? p.c : p.m), max: 100, threshold: 90, thresholdLabel: "90%", format: pct, tick: pct }
	}

	const vals = series.map(cfg.pick)
	const known = vals.filter((v): v is number => v !== undefined)
	const last = vals.length ? vals[vals.length - 1] : undefined
	const lastKnown = known.length ? known[known.length - 1] : undefined
	const stroke = !service || service.status === "UP" ? "var(--brand)" : STATE[service.status].css
	const secondsSinceKnown = (() => {
		for (let i = vals.length - 1; i >= 0; i--) if (vals[i] !== undefined) return Math.round((series[series.length - 1].t - series[i].t) / 1000)
		return 0
	})()

	return (
		<Panel
			title={label ? `${label} · ${t`Last 15 minutes`}` : t`Last 15 minutes`}
			right={
				selected ? (
					<div className="flex max-w-full gap-0.5 overflow-x-auto rounded-lg border bg-background p-0.5" role="tablist">
						{tabs.map((x) => (
							<button
								key={x.key}
								role="tab"
								type="button"
								aria-selected={active === x.key}
								onClick={() => setMetric(x.key)}
								className={cn(
									"h-7 whitespace-nowrap rounded-md px-3 text-sm transition-colors",
									active === x.key ? "bg-accent text-foreground shadow-[inset_0_1px_0_rgb(255_255_255/0.06)]" : "text-muted-foreground hover:text-foreground"
								)}
							>
								{x.label}
							</button>
						))}
					</div>
				) : undefined
			}
		>
			{!selected ? (
				<p className="py-12 text-center text-sm text-muted-foreground">{t`Select a service or node`}</p>
			) : known.length === 0 ? (
				<p className="py-12 text-center text-sm text-muted-foreground">{t`No probe data yet`}</p>
			) : (
				<>
					<div className="mb-2 flex flex-wrap items-baseline gap-x-4">
						<span className={cn("text-[2rem] leading-9 font-medium tracking-[-0.01em] tabular-nums", last === undefined && "text-down")}>
							{last === undefined ? t`No response` : cfg.format(last)}
						</span>
						<span className="text-sm text-muted-foreground">
							{last === undefined
								? t`last answer ${cfg.format(lastKnown as number)}, ${secondsSinceKnown} s ago`
								: cfg.threshold !== undefined
									? last > cfg.threshold
										? t`above the ${cfg.thresholdLabel} threshold`
										: t`below the ${cfg.thresholdLabel} threshold`
									: ""}
						</span>
					</div>
					<TrendChart points={series} color={stroke} {...cfg} />
				</>
			)}
		</Panel>
	)
}

const RULE_LABEL: Record<string, string> = {
	service_down: "service down",
	service_degraded: "service degraded",
	service_unreachable: "service unreachable (probes failing)",
	pod_link_failed: "Pod → Service connection blocked",
	service_errors: "probe errors",
	service_latency_high: "response time high",
	pod_cpu_high: "pod CPU high",
	pod_memory_high: "pod memory high",
	pod_unhealthy: "pod unhealthy",
	node_down: "node down",
	node_cpu_high: "node CPU high",
	node_memory_high: "node memory high",
	node_pressure: "node pressure",
}

function IncidentList({ events }: { events: ReturnType<typeof incidentEvents> }) {
	const { t } = useLingui()
	if (events.length === 0) {
		return <p className="py-8 text-center text-sm text-muted-foreground">{t`No incidents`}</p>
	}
	return (
		<ul className="-mx-2 grid max-h-[40rem] overflow-y-auto overflow-x-hidden px-2">
			{events.map((ev) => (
				<IncidentRow key={ev.id} ev={ev} />
			))}
		</ul>
	)
}

function IncidentRow({ ev }: { ev: ReturnType<typeof incidentEvents>[number] }) {
	const { t } = useLingui()
	const inc: Incident = ev.incident
	const time = new Date(ev.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false })
	const recovered = ev.kind === "RECOVERED"
	const ongoing = inc.open && !recovered
	const sev = SEVERITY[ev.kind]
	const sevLabel = { CRITICAL: t`Critical`, WARNING: t`Warning`, RECOVERED: t`Recovered` }
	const downFor =
		recovered && inc.recoveredAt ? formatDuration(Math.round((new Date(inc.recoveredAt).getTime() - new Date(inc.startedAt).getTime()) / 1000)) : null
	return (
		<li className="grid min-w-0 grid-cols-[4rem_minmax(0,1fr)] gap-3 border-t py-3 first:border-t-0 first:pt-0">
			<span className="pt-0.5 font-mono text-xs tabular-nums text-muted-foreground">{time}</span>
			<div className="min-w-0">
				<div className="flex items-center gap-2">
					<span className={cn("shrink-0 text-xs", sev.text)}>{sevLabel[ev.kind]}</span>
					<span className="min-w-0 truncate font-medium" title={inc.target}>
						{inc.target}
					</span>
					{ongoing && <span className="ms-auto shrink-0 text-xs text-muted-foreground">{t`ongoing`}</span>}
				</div>
				<div className="mt-0.5 text-sm text-muted-foreground">
					{RULE_LABEL[inc.rule] ?? inc.rule}
					{inc.observed ? ` · ${inc.observed}` : ""}
				</div>
				<div className="mt-1 break-words font-mono text-xs text-muted-foreground/80">
					{recovered ? (inc.note ? inc.note : `${t`recovered after`} ${downFor}`) : `${t`rule`}: ${inc.threshold} · ${inc.window}`}
				</div>
			</div>
		</li>
	)
}
