import { memo, Suspense } from "react"
import OverviewPage from "@/components/routes/overview/overview"

export default memo(() => (
	<Suspense>
		<OverviewPage />
	</Suspense>
))
