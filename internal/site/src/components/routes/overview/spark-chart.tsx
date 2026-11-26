import { useEffect, useRef, useState } from "react"
import type { Point } from "@/lib/overview"

interface Props {
	points: Point[]
	/** Value of one point, or undefined when that point carries no data (a gap in the line). */
	pick: (p: Point) => number | undefined
	/** Fixed upper bound of the y axis (e.g. 100 for percentages). Otherwise it follows the data. */
	max?: number
	/** Horizontal reference line, dashed in the warning colour. */
	threshold?: number
	thresholdLabel?: string
	/** Formats a value for the hover readout. */
	format: (v: number) => string
	/** Formats a y axis tick. */
	tick?: (v: number) => string
	/** Any CSS colour, normally a design token like `var(--brand)`. */
	color: string
	height?: number
}

const WINDOW_MS = 15 * 60 * 1000
const GAP_MS = 15_000
const PAD = { l: 44, r: 12, t: 16, b: 28 }

/** Monotone cubic path, so a curve never overshoots the data between two points. */
function smooth(pts: [number, number][]) {
	const n = pts.length
	if (n < 2) return ""
	const dx: number[] = []
	const m: number[] = []
	const t: number[] = []
	for (let i = 0; i < n - 1; i++) {
		dx[i] = pts[i + 1][0] - pts[i][0] || 1e-6
		m[i] = (pts[i + 1][1] - pts[i][1]) / dx[i]
	}
	t[0] = m[0]
	t[n - 1] = m[n - 2]
	for (let i = 1; i < n - 1; i++) t[i] = m[i - 1] * m[i] <= 0 ? 0 : (m[i - 1] + m[i]) / 2
	let d = `M${pts[0][0].toFixed(1)},${pts[0][1].toFixed(1)}`
	for (let i = 0; i < n - 1; i++) {
		const h = dx[i] / 3
		d += `C${(pts[i][0] + h).toFixed(1)},${(pts[i][1] + t[i] * h).toFixed(1)} ${(pts[i + 1][0] - h).toFixed(1)},${(pts[i + 1][1] - t[i + 1] * h).toFixed(1)} ${pts[i + 1][0].toFixed(1)},${pts[i + 1][1].toFixed(1)}`
	}
	return d
}

/** Dependency-free line chart over a fixed 15 minute window. It is the one large chart on the overview. */
export default function TrendChart({ points, pick, max, threshold, thresholdLabel, format, tick, color, height = 300 }: Props) {
	const box = useRef<HTMLDivElement>(null)
	const [width, setWidth] = useState(720)
	const [hover, setHover] = useState<number | null>(null)

	useEffect(() => {
		const el = box.current
		if (!el) return
		const ro = new ResizeObserver(() => setWidth(Math.max(280, el.clientWidth)))
		ro.observe(el)
		setWidth(Math.max(280, el.clientWidth))
		return () => ro.disconnect()
	}, [])

	const now = points.length ? points[points.length - 1].t : Date.now()
	const start = now - WINDOW_MS
	const vals = points.map(pick)
	const known = vals.filter((v): v is number => v !== undefined)
	const top = max ?? Math.max(1, ...known, (threshold ?? 0) * 1.2) * 1.15
	const innerW = width - PAD.l - PAD.r
	const innerH = height - PAD.t - PAD.b
	const x = (t: number) => PAD.l + ((t - start) / WINDOW_MS) * innerW
	const y = (v: number) => PAD.t + innerH * (1 - Math.min(v, top) / top)

	// split into runs; a run ends where the data stops for more than GAP_MS
	const runs: [number, number][][] = []
	let cur: [number, number][] = []
	let prevT = 0
	points.forEach((p, i) => {
		const v = vals[i]
		if (v === undefined || (cur.length && p.t - prevT > GAP_MS)) {
			if (cur.length) runs.push(cur)
			cur = []
		}
		if (v !== undefined) {
			cur.push([x(p.t), y(v)])
			prevT = p.t
		}
	})
	if (cur.length) runs.push(cur)

	const lastIdx = vals.length - 1
	const ended = vals.length > 0 && vals[lastIdx] === undefined
	let gapFrom = -1
	if (ended) {
		gapFrom = lastIdx
		while (gapFrom > 0 && vals[gapFrom - 1] === undefined) gapFrom--
	}
	const tip = hover !== null ? points[hover] : null
	const fmtTick = tick ?? ((v: number) => String(Math.round(v)))
	const lastRun = runs[runs.length - 1]
	const endPt = !ended && lastRun ? lastRun[lastRun.length - 1] : null

	return (
		<div ref={box} className="relative" style={{ height }}>
			{known.length === 0 ? (
				<div className="h-full grid place-items-center text-sm text-muted-foreground" />
			) : (
				<svg
					width={width}
					height={height}
					className="block overflow-visible"
					role="img"
					onMouseLeave={() => setHover(null)}
					onMouseMove={(e) => {
						const r = e.currentTarget.getBoundingClientRect()
						const t = start + ((e.clientX - r.left - PAD.l) / innerW) * WINDOW_MS
						let best = 0
						for (let i = 1; i < points.length; i++) if (Math.abs(points[i].t - t) < Math.abs(points[best].t - t)) best = i
						setHover(best)
					}}
				>
					{[0, 0.5, 1].map((f) => (
						<g key={f}>
							<line x1={PAD.l} x2={width - PAD.r} y1={y(top * f)} y2={y(top * f)} stroke="var(--border)" />
							<text x={PAD.l - 8} y={y(top * f) + 4} textAnchor="end" className="fill-muted-foreground font-mono text-[11px]">
								{fmtTick(top * f)}
							</text>
						</g>
					))}
					{[
						["−15 min", 0, "start"],
						["−10", 1 / 3, "middle"],
						["−5", 2 / 3, "middle"],
						["now", 1, "end"],
					].map(([label, f, anchor]) => (
						<text
							key={label as string}
							x={PAD.l + innerW * (f as number)}
							y={height - 6}
							textAnchor={anchor as "start"}
							className="fill-muted-foreground font-mono text-[11px]"
						>
							{label}
						</text>
					))}
					{threshold !== undefined && threshold < top && (
						<g>
							<line x1={PAD.l} x2={width - PAD.r} y1={y(threshold)} y2={y(threshold)} stroke="var(--warn)" strokeDasharray="4 4" />
							<text x={width - PAD.r} y={y(threshold) - 6} textAnchor="end" className="fill-warn font-mono text-[11px]">
								{thresholdLabel}
							</text>
						</g>
					)}
					{ended && gapFrom >= 0 && (
						<g>
							<rect x={x(points[gapFrom].t)} y={PAD.t + 20} width={Math.max(0, width - PAD.r - x(points[gapFrom].t))} height={innerH - 20} fill="var(--down)" opacity="0.08" />
							<text x={width - PAD.r} y={PAD.t + 14} textAnchor="end" className="fill-down font-mono text-[11px]">
								no data
							</text>
						</g>
					)}
					{runs.map((run) =>
						run.length > 1 ? (
							<g key={run[0][0]}>
								<path
									d={`${smooth(run)}L${run[run.length - 1][0].toFixed(1)},${y(0)}L${run[0][0].toFixed(1)},${y(0)}Z`}
									fill={color}
									opacity="0.1"
								/>
								<path d={smooth(run)} fill="none" stroke={color} strokeWidth="2" strokeLinecap="round" />
							</g>
						) : null
					)}
					{endPt && (
						<g>
							<circle cx={endPt[0]} cy={endPt[1]} r="8" fill={color} opacity="0.2" />
							<circle cx={endPt[0]} cy={endPt[1]} r="4" fill={color} />
						</g>
					)}
					{tip && (
						<line x1={x(tip.t)} x2={x(tip.t)} y1={PAD.t} y2={height - PAD.b} stroke="var(--input)" />
					)}
				</svg>
			)}
			{tip && (
				<div
					className="pointer-events-none absolute top-3 rounded-md border bg-popover px-3 py-2 font-mono text-xs shadow-md"
					style={{ left: Math.min(width - 120, Math.max(0, x(tip.t) + 12)) }}
				>
					{new Date(tip.t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false })}
					<br />
					{vals[hover as number] === undefined ? "no data" : format(vals[hover as number] as number)}
				</div>
			)}
		</div>
	)
}
