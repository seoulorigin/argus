import { t } from "@lingui/core/macro"
import { useStore } from "@nanostores/react"
import { useEffect, useMemo, useState } from "react"
import { UserAuthForm } from "@/components/login/auth-form"
import { pb } from "@/lib/api"
import { $authenticated } from "@/lib/stores"
import { Logo } from "../logo"
import { ModeToggle } from "../mode-toggle"
import { $router } from "../router"
import ForgotPassword from "./forgot-pass-form"
import { loadLogin } from "./load-login"
import { OtpRequestForm } from "./otp-forms"

export default function () {
	const page = useStore($router)
	const [login, setLogin] = useState<Awaited<ReturnType<typeof loadLogin>>>()
	const isFirstRun = login?.firstRun ?? false

	// Load once on mount; switching between login routes reuses the result.
	useEffect(() => {
		const controller = new AbortController()
		const autoLogin = !!globalThis.ARGUS.AUTO_LOGIN
		// keep the title neutral while automatic authentication may still succeed
		document.title = autoLogin ? "Argus" : `${t`Login`} / Argus`
		loadLogin(
			pb,
			{
				route: $router.get()?.route,
				search: window.location.search,
				loggedOut: !!sessionStorage.getItem("lo"),
				autoLogin,
			},
			controller.signal
		)
			.then((result) => {
				if (controller.signal.aborted) return
				if (result.authenticated) {
					$authenticated.set(true)
					return
				}
				document.title = `${t`Login`} / Argus`
				setLogin(result)
			})
			.catch((error) => {
				if (!controller.signal.aborted) console.error(error)
			})
		return () => controller.abort()
	}, [])

	const subtitle = useMemo(() => {
		if (isFirstRun) {
			return t`Please create an admin account`
		} else if (page?.route === "forgot_password") {
			return t`Enter email address to reset password`
		} else if (page?.route === "request_otp") {
			return t`Request a one-time password`
		} else {
			return t`Please sign in to your account`
		}
	}, [isFirstRun, page])

	if (!login) {
		return null
	}

	return (
		<div className="min-h-svh grid items-center py-12 px-4">
			<div className="grid gap-6 w-full mx-auto rounded-2xl border bg-card p-8" style={{ maxWidth: "24rem" }}>
				<div className="absolute top-3 right-3">
					<ModeToggle />
				</div>
				<div>
					<h1 className="mb-2">
						{/* Logo's aria-label provides the heading text */}
						<Logo className="h-7 fill-foreground" />
					</h1>
					<p className="text-sm text-muted-foreground">{subtitle}</p>
				</div>
				{page?.route === "forgot_password" ? (
					<ForgotPassword />
				) : page?.route === "request_otp" ? (
					<OtpRequestForm />
				) : (
					<UserAuthForm isFirstRun={isFirstRun} authMethods={login.authMethods} />
				)}
			</div>
		</div>
	)
}
