export function Logo({ className }: { className?: string }) {
	return (
		<svg role="img" aria-label="Argus" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 96 28" className={className}>
			<g fill="none" stroke="currentColor" strokeWidth="2.25" strokeLinejoin="round">
				<path d="M2.5 14C5.8 8.6 9.9 5.8 14 5.8s8.2 2.8 11.5 8.2c-3.3 5.4-7.4 8.2-11.5 8.2S5.8 19.4 2.5 14Z" />
			</g>
			<circle cx="14" cy="14" r="3.6" fill="currentColor" />
			<text x="34" y="21" fill="currentColor" fontFamily="Geist, ui-sans-serif, system-ui, sans-serif" fontWeight="500" fontSize="20">
				Argus
			</text>
		</svg>
	)
}
