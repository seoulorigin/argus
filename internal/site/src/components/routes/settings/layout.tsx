import { t } from "@lingui/core/macro"
import { Trans, useLingui } from "@lingui/react/macro"
import { useStore } from "@nanostores/react"
import { getPagePath, redirectPage } from "@nanostores/router"
import {
	FileSlidersIcon,
	FingerprintIcon,
	SettingsIcon,
} from "lucide-react"
import { lazy, useEffect } from "react"
import { $router } from "@/components/router.tsx"
import { Card } from "@/components/ui/card"
import { toast } from "@/components/ui/use-toast.ts"
import { saveUserSettings } from "@/lib/api"
import { $userSettings } from "@/lib/stores.ts"
import type { UserSettings } from "@/types"
import { SidebarNav } from "./sidebar-nav.tsx"

const generalSettingsImport = () => import("./general.tsx")
const configYamlSettingsImport = () => import("./config-yaml.tsx")
const fingerprintsSettingsImport = () => import("./tokens-fingerprints.tsx")

const GeneralSettings = lazy(generalSettingsImport)
const ConfigYamlSettings = lazy(configYamlSettingsImport)
const FingerprintsSettings = lazy(fingerprintsSettingsImport)

export async function saveSettings(newSettings: Partial<UserSettings>) {
	try {
		await saveUserSettings(newSettings)
		toast({
			title: t`Settings saved`,
			description: t`Your user settings have been updated.`,
		})
	} catch (e) {
		console.error("save settings", e)
		toast({
			title: t`Failed to save settings`,
			description: t`Check logs for more details.`,
			variant: "destructive",
		})
	}
}

export default function SettingsLayout() {
	const { t } = useLingui()

	const sidebarNavItems = [
		{
			title: t({ message: `General`, comment: "Context: General settings" }),
			href: getPagePath($router, "settings", { name: "general" }),
			icon: SettingsIcon,
		},
		{
			title: t`Tokens & Fingerprints`,
			href: getPagePath($router, "settings", { name: "tokens" }),
			icon: FingerprintIcon,
			noReadOnly: true,
			preload: fingerprintsSettingsImport,
		},
		{
			title: t`YAML Config`,
			href: getPagePath($router, "settings", { name: "config" }),
			icon: FileSlidersIcon,
			admin: true,
			preload: configYamlSettingsImport,
		},
	]

	const page = useStore($router)

	// biome-ignore lint/correctness/useExhaustiveDependencies: no dependencies
	useEffect(() => {
		document.title = `${t`Settings`} / Argus`
		// @ts-expect-error redirect to account page if no page is specified
		if (!page?.params?.name) {
			redirectPage($router, "settings", { name: "general" })
		}
	}, [])

	return (
		<div className="grid gap-8 min-h-96">
			<div>
				<h1 className="text-[1.75rem] leading-8 font-medium tracking-[-0.01em]">
					<Trans>Settings</Trans>
				</h1>
				<p className="text-sm text-muted-foreground mt-1">
					<Trans>Display, units and access for this Argus hub.</Trans>
				</p>
			</div>
			<div className="flex flex-col gap-4 md:flex-row md:gap-12">
				<aside className="md:w-56 md:shrink-0">
					<SidebarNav items={sidebarNavItems} />
				</aside>
				<Card className="flex-1 min-w-0 p-6 max-w-4xl">
					{/* @ts-ignore */}
					<SettingsContent name={page?.params?.name ?? "general"} />
				</Card>
			</div>
		</div>
	)
}

function SettingsContent({ name }: { name: string }) {
	const userSettings = useStore($userSettings)

	switch (name) {
		case "general":
			return <GeneralSettings userSettings={userSettings} />
		case "config":
			return <ConfigYamlSettings />
		case "tokens":
			return <FingerprintsSettings />
	}
}
