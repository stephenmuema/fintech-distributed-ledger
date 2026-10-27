import { lazy } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'

// The internal operations console. Pages load on demand; the checkout is the one page a
// payer outside the business opens (a payment link).
const ConsoleLayout = lazy(() => import('./console/ConsoleLayout.tsx'))
const Overview = lazy(() => import('./console/pages/Overview.tsx'))
const Analytics = lazy(() => import('./console/pages/Analytics.tsx'))
const Customers = lazy(() => import('./console/pages/Customers.tsx'))
const Accounts = lazy(() => import('./console/pages/Accounts.tsx'))
const Transact = lazy(() => import('./console/pages/Transact.tsx'))
const Transactions = lazy(() => import('./console/pages/Transactions.tsx'))
const TransactionSetup = lazy(() => import('./console/pages/TransactionSetup.tsx'))
const LocalTables = lazy(() => import('./console/pages/LocalTables.tsx'))
const Enquiries = lazy(() => import('./console/pages/Enquiries.tsx'))
const Payouts = lazy(() => import('./console/pages/Payouts.tsx'))
const CloseOfBusiness = lazy(() => import('./console/pages/CloseOfBusiness.tsx'))
const GeneralLedger = lazy(() => import('./console/pages/GeneralLedger.tsx'))
const Controls = lazy(() => import('./console/pages/Controls.tsx'))
const Reconciliation = lazy(() => import('./console/pages/Reconciliation.tsx'))
const Operations = lazy(() => import('./console/pages/Operations.tsx'))
const Suspense = lazy(() => import('./console/pages/Suspense.tsx'))
const SuspenseTypes = lazy(() => import('./console/pages/SuspenseTypes.tsx'))
const Nostro = lazy(() => import('./console/pages/Nostro.tsx'))
const Vostro = lazy(() => import('./console/pages/Vostro.tsx'))
const Extensions = lazy(() => import('./console/pages/Extensions.tsx'))
const Channels = lazy(() => import('./console/pages/Channels.tsx'))
const Operators = lazy(() => import('./console/pages/Operators.tsx'))
const Login = lazy(() => import('./console/Login.tsx'))
const Checkout = lazy(() => import('./checkout/Checkout.tsx'))

export default function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/console" replace />} />
      <Route path="/checkout/:code" element={<Checkout />} />
      <Route path="/console/login" element={<Login />} />
      <Route path="/console" element={<ConsoleLayout />}>
        <Route index element={<Overview />} />
        <Route path="analytics" element={<Analytics />} />
        <Route path="customers" element={<Customers />} />
        <Route path="accounts" element={<Accounts />} />
        <Route path="accounts/:accountNumber" element={<Accounts />} />
        <Route path="transact" element={<Transact />} />
        <Route path="transactions" element={<Transactions />} />
        <Route path="transactions/:reference" element={<Transactions />} />
        <Route path="enquiries" element={<Enquiries />} />
        <Route path="setup/transactions" element={<TransactionSetup />} />
        <Route path="setup/local-tables" element={<LocalTables />} />
        <Route path="payouts" element={<Payouts />} />
        <Route path="cob" element={<CloseOfBusiness />} />
        <Route path="ledger" element={<GeneralLedger />} />
        <Route path="controls" element={<Controls />} />
        <Route path="reconciliation" element={<Reconciliation />} />
        <Route path="operations" element={<Operations />} />
        <Route path="suspense" element={<Suspense />} />
        <Route path="suspense/types" element={<SuspenseTypes />} />
        <Route path="nostro" element={<Nostro />} />
        <Route path="vostro" element={<Vostro />} />
        <Route path="extensions" element={<Extensions />} />
        <Route path="channels" element={<Channels />} />
        <Route path="operators" element={<Operators />} />
      </Route>
      <Route path="*" element={<Navigate to="/console" replace />} />
    </Routes>
  )
}
