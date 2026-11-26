import type { Messages } from "@lingui/core"
import { i18n } from "@lingui/core"
import { t } from "@lingui/core/macro"
import { detect, fromStorage } from "@lingui/detect-locale"
import languages from "@/lib/languages"
import { messages as enMessages } from "@/locales/en/en"
import { BatteryState } from "./enums"
import { $direction } from "./stores"

// activates locale
function activateLocale(locale: string, messages: Messages = enMessages) {
	i18n.load(locale, messages)
	i18n.activate(locale)
	document.documentElement.lang = locale
	localStorage.setItem("lang", locale)
	$direction.set("ltr")
}

// dynamically loads translations for the given locale
export async function dynamicActivate(locale: string) {
	if (locale === "en") {
		activateLocale(locale)
	} else {
		try {
			const { messages }: { messages: Messages } = await import(`../locales/${locale}/${locale}.ts`)
			activateLocale(locale, messages)
		} catch (error) {
			console.error(`Error loading ${locale}`, error)
			activateLocale("en")
		}
	}
}

// English unless the user picked another language in Settings (kept in localStorage).
export function getLocale() {
	const locale = detect(fromStorage("lang"), "en")
	return languages.some((l) => l[0] === locale) ? (locale as string) : "en"
}

////////////////////////////////////////////////////////

export const batteryStateTranslations = {
	[BatteryState.Unknown]: () => t({ message: "Unknown", comment: "Context: Battery state" }),
	[BatteryState.Empty]: () => t({ message: "Empty", comment: "Context: Battery state" }),
	[BatteryState.Full]: () => t({ message: "Full", comment: "Context: Battery state" }),
	[BatteryState.Charging]: () => t({ message: "Charging", comment: "Context: Battery state" }),
	[BatteryState.Discharging]: () => t({ message: "Discharging", comment: "Context: Battery state" }),
	[BatteryState.Idle]: () => t({ message: "Idle", comment: "Context: Battery state" }),
} as const
