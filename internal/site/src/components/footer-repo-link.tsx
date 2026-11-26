import { useStore } from "@nanostores/react"
import { GithubIcon } from "lucide-react"
import { $newVersion } from "@/lib/stores"
import { Trans } from "@lingui/react/macro"

export function FooterRepoLink() {
	const newVersion = useStore($newVersion)
	return (
		<div className="flex gap-3 justify-end items-center mt-8 text-xs font-mono">
			<a
				href="https://github.com/seoulorigin/argus"
				target="_blank"
				className="flex items-center gap-1.5 text-muted-foreground hover:text-foreground duration-75"
				rel="noopener"
			>
				<GithubIcon className="size-3.5" /> GitHub
			</a>
			
			<a
				href="https://github.com/seoulorigin/argus/releases"
				target="_blank"
				className="text-muted-foreground hover:text-foreground duration-75"
				rel="noopener"
			>
				Argus {globalThis.ARGUS.HUB_VERSION}
			</a>
			{newVersion?.v && (
				<>
					
					<a
						href={newVersion.url}
						target="_blank"
						className="text-warn hover:text-warn duration-75"
						rel="noopener"
					>
						<Trans context="New version available">{newVersion.v} available</Trans>
					</a>
				</>
			)}
		</div>
	)
}
