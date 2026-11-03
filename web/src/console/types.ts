// Response shapes of the Zirnis API (camelCase JSON, enums as strings).

export type SystemDates = {
  today: string
  lastWorkingDay: string
  nextWorkingDay: string
  status: 'Online' | 'CloseOfBusiness'
  onlineBookingDate: string
  isMonthEnd: boolean
}

export type Customer = {
  id: string
  customerNumber: number
  mnemonic: string
  type: 'Individual' | 'Business' | 'FinancialInstitution'
  sector: number
  accountOfficer: number
  legalName: string
  email: string
  phone?: string
  country: string
  kycStatus: string
  tier: number
  kycRejectionReason?: string
  version: number
  createdAt: string
  contactChangedAt?: string
}

export type Account = {
  id: string
  accountNumber: string
  customerId: string
  title: string
  category: number
  currency: string
  workingBalance: number
  lockedAmount: number
  onlineActualBalance: number
  restriction: string
  restrictionReason?: string
  openingDate: string
  closedAt?: string
  closureReason?: string
}

/** One field changed on a record: who changed it, when, from what to what. */
export type RecordChange = {
  id: number
  entityType: string
  entityId: string
  field: string
  oldValue?: string | null
  newValue?: string | null
  changedBy: string
  changedAt: string
  correlationId?: string
}

export type StatementLine = {
  entryId: string
  transactionReference: string
  category: string
  description: string
  direction: 'Debit' | 'Credit'
  amount: number
  balanceAfter: number
  bookingDate: string
  valueDate: string
  postedAt: string
}

export type Attempt = { provider: string; outcome: string; providerReference?: string; errorCode?: string; attemptedAt: string }

export type Payment = {
  id: string
  accountId: string
  amount: number
  currency: string
  method: 'Card' | 'MobileMoney' | 'Stablecoin'
  paymentSource: string
  status: string
  provider?: string
  providerReference?: string
  fee: number
  refundedAmount: number
  failureCode?: string
  failureMessage?: string
  createdAt: string
  attempts: Attempt[]
}

export type Payout = {
  id: string
  accountId: string
  amount: number
  fee: number
  currency: string
  rail: string
  destinationAccount: string
  destinationName: string
  status: string
  provider?: string
  providerReference?: string
  submissionAttempts: number
  nextAttemptAt?: string
  failureCode?: string
  reviewedBy?: string
  /** No provider confirmed the outcome: operations settle it (paid, or returned). */
  escalatedAt?: string
  validationRequestedAt?: string
  createdAt: string
  updatedAt: string
}

export type CobJob = { id: string; name: string; stage: string; order: number; status: string; attempts: number; recordsProcessed: number; summary?: string; error?: string; startedAt?: string; finishedAt?: string }

export type CobRun = {
  id: string
  businessDate: string
  isMonthEnd: boolean
  status: string
  startedBy: string
  startedAt: string
  completedAt?: string
  newBusinessDate?: string
  report?: string
  jobs: CobJob[]
}

export type GlLine = { glCode: string; name: string; type: string; section: string; group: string; currency: string; balance: number; balanceLcy: number }

export type BalanceSheet = { asOf?: string; localCurrency: string; assets: number; liabilities: number; equity: number; currentYearEarnings: number; balances: boolean; lines: GlLine[] }

export type ProfitAndLoss = { asOf?: string; localCurrency: string; income: number; expenses: number; netProfit: number; lines: GlLine[] }

export type TrialBalanceLine = { currency: string; debitNormalTotal: number; creditNormalTotal: number; balanced: boolean }

export type LedgerAccount = { id: string; code: string; accountNumber: string; name: string; type: string; category: number; glCode: string; currency: string; balance: number }

export type TransactionLeg = { accountNumber: string; accountName: string; glCode: string; direction: 'Debit' | 'Credit'; amount: number; currency: string; balanceAfter: number }

/** A journal entry with its legs (T24: STMT.ENTRY), as the GL enquiries return it. */
export type JournalEntryView = {
  id: string
  transactionReference: string
  reference: string
  category: string
  description: string
  bookingDate: string
  valueDate: string
  postedAt: string
  legs: TransactionLeg[]
}

export type ChargeType = { id: string; code: string; description: string; basisPoints: number; version: number }

export type InterestCondition = { id: string; category: number; currency: string; annualRate: number; version: number }

export type OnboardingPolicy = { id?: string; kycRequired: boolean; reason: string; updatedBy: string; updatedAt: string }

export type ParameterChange = { id: string; type: string; key: string; payload: string; status: string; inputter: string; inputAt: string; authoriser?: string }

export type Adjustment = {
  id: string
  currency: string
  narrative: string
  lines: { accountNumber: string; direction: string; amount: number }[]
  status: string
  inputter: string
  inputAt: string
  authoriser?: string
  rejectionReason?: string
}

export type ReconItem = { id: string; kind: string; providerReference: string; currency: string; reportAmount?: number; reportFee?: number; ledgerAmount?: number; outcome: string; resolved: boolean; resolutionNote?: string; resolvedBy?: string }

export type ReconRun = { id: string; provider: string; reportDate: string; createdBy: string; createdAt: string; settlementEntryId?: string; settledAt?: string; items: ReconItem[] }

// module serves the provider ('' for the sandbox simulator); notRoutable says why nothing can reach it.
export type ProviderStatus = { name: string; live: boolean; circuit: string; simulatedOutage: boolean; module: string; notRoutable?: string }

export type AuditEvent = { sequence: number; actor: string; action: string; entityType: string; entityId: string; data?: string; occurredAt: string; hash: string; previousHash: string }

export type OutboxMessage = { id: string; type: string; customerId: string; status: string; attempts: number; occurredAt: string; nextAttemptAt?: string; lastError?: string }

// ---- AC.SUSPENSE ----

export type LocalField = { name: string; label: string; type: 'text' | 'number' | 'date' | 'choice'; required: boolean; pattern?: string; options?: string[] }

export type SuspenseTypeSpec = {
  name: string
  description: string
  side: 'Debit' | 'Credit'
  currencies: string[]
  warnAfterDays: number
  escalateAfterDays: number
  cobPolicy: 'Warn' | 'HaltCob'
  writeOffAfterDays: number
  writeOffCategory: number
  allowedTargets: string[]
  hooks: string[]
  autoClearAt: number
  localFields: LocalField[]
  active?: boolean
}

export type SuspenseType = SuspenseTypeSpec & { id: string; version: number; code: string; category: number; glCode: string; builtIn: boolean; active: boolean; createdAt: string }

export type SuspenseItem = {
  id: string
  version: number
  typeCode: string
  accountNumber: string
  side: 'Debit' | 'Credit'
  currency: string
  amount: number
  openAmount: number
  status: string
  reference: string
  narrative: string
  sourceType: string
  sourceId: string
  contraAccountNumber?: string
  journalEntryId?: string
  openedOn: string
  openedAt: string
  inputter: string
  authoriser?: string
  assignedTo?: string
  attributes: Record<string, string>
  classifiedAt?: string
  escalated: boolean
  closedOn?: string
  ageDays: number
  ageBucket: string
  pendingClearing: boolean
}

export type SuspenseClearing = {
  id: string
  itemId: string
  typeCode: string
  kind: 'Clear' | 'WriteOff'
  targetAccountNumber: string
  currency: string
  amount: number
  narrative: string
  confidence?: number
  status: string
  inputter: string
  inputAt: string
  authoriser?: string
  authorisedAt?: string
  rejectionReason?: string
  journalEntryId?: string
}

export type ExtensionCall = { id: string; extension: string; hook: string; remote: boolean; subject: string; outcome: string; latencyMs: number; error?: string; calledAt: string }

export type HistoryRecord = { id: number; entityType: string; currNo: number; snapshot: string; changedBy: string; changedAt: string }

export type JournalLine = { id: string; accountId: string; direction: 'Debit' | 'Credit'; amount: number; currency: string; balanceAfter: number }

export type JournalEntry = { id: string; reference: string; transactionReference: string; category: string; description: string; bookingDate: string; lines: JournalLine[] }

export type SuspenseItemDetail = {
  item: SuspenseItem
  type: SuspenseType
  entry?: JournalEntry
  clearings: SuspenseClearing[]
  extensionCalls: ExtensionCall[]
  history: HistoryRecord[]
  related: SuspenseItem[]
  events: SuspenseEvent[]
}

export type AgeingRow = {
  type: string
  name: string
  side: string
  currency: string
  accountNumber: string
  balance: number
  openItems: number
  openAmount: number
  buckets: Record<string, number>
  overWarning: number
  escalated: number
  oldestDays: number
  cobPolicy: string
}

export type SuspenseDrift = { type: string; accountNumber: string; currency: string; balance: number; openItems: number }

export type HookRun = { classified: number; proposed: number; autoCleared: number; rejected: number }

// ---- EB.API ----

export type Extension = {
  name: string
  kind: 'InProcess' | 'Remote'
  hooks: string[]
  url?: string
  secretEnv?: string
  timeoutMs?: number
  failurePolicy: string
  enabled: boolean
  version: number
  circuit: string
  consecutiveFailures: number
  lastError?: string
  lastCallAt?: string
  secretPresent?: boolean
}

export type HookEndpoint = { id: string; name: string; url: string; hooks: string[]; secretEnv: string; timeoutMs: number; failurePolicy: string; enabled: boolean }

// ---- NV ----

export type Correspondent = { id: string; version: number; code: string; name: string; kind: string; bic: string; bankCode: string; country: string; provider: string; customerId?: string; active: boolean }

export type Proof = {
  asOf: string
  ledgerBalance: number
  openingBreak: number
  statementClosing: number
  unmatchedStatementCredits: number
  unmatchedStatementDebits: number
  unmatchedBookDebits: number
  unmatchedBookCredits: number
  difference: number
  closes: boolean
}

export type NostroStatement = {
  id: string
  nostroId: string
  nostroCode: string
  format: string
  statementNumber: string
  currency: string
  openingBalance?: number
  closingBalance?: number
  fromDate: string
  toDate: string
  lineCount: number
  status: string
  proof?: Proof
  receivedBy: string
  receivedAt: string
}

export type Nostro = {
  id: string
  version: number
  code: string
  correspondentCode: string
  correspondentName: string
  currency: string
  externalAccount: string
  ledgerAccountNumber: string
  statementSource: string
  valueDateWindow: number
  parkAfterDays: number
  creditSuspenseType: string
  debitSuspenseType: string
  hooks: string[]
  proofPolicy: string
  reconciledFrom?: string
  openingBreak: number
  active: boolean
  ledgerBalance: number
  lastStatement?: NostroStatement
  unmatchedStatementLines: number
  unmatchedBookEntries: number
  live: boolean
  liveBalance?: number
  liveDifference?: number
  liveError?: string
}

export type NostroLine = {
  id: string
  lineNo: number
  valueDate: string
  entryDate?: string
  direction: 'Credit' | 'Debit'
  amount: number
  bankReference: string
  customerReference: string
  narrative: string
  status: string
  matchId?: string
  suspenseItemId?: string
}

export type BookLine = { lineId: string; entryId: string; transactionReference: string; reference: string; description: string; direction: 'Debit' | 'Credit'; amount: number; bookingDate: string; valueDate: string }

export type NostroMatch = { id: string; method: string; matchedBy: string; confidence?: number; createdAt: string; statementLineIds: string[]; bookLines: BookLine[] }

export type StatementDetail = {
  statement: NostroStatement
  lines: NostroLine[]
  openBookEntries: BookLine[]
  matches: NostroMatch[]
  nostroCode: string
  reconciliation?: { byReference: number; byAmountAndDate: number; byExtension: number; parked: number }
}

export type Vostro = Account & { holderName: string; customerNumber: number; correspondent: string }

// ---- CH ----

export type SuspenseEvent = {
  id: string
  itemId: string
  typeCode: string
  status: 'Pending' | 'Delivering' | 'Delivered' | 'Held' | 'Failed'
  attempts: number
  nextAttemptAt: string
  extension?: string
  outcome?: string
  lastError?: string
  createdAt: string
  deliveredAt?: string
}

export type ChannelSpec = {
  name: string
  description: string
  currencies: string[]
  inward: boolean
  outward: boolean
  extension: string
  settlementAccounts: Record<string, string>
  localFields: LocalField[]
  minAmount: number
  maxAmount: number
  stp: boolean
  autoClearAt: number
  escalateAfterDays: number
  settlementHoldDays: number
  active?: boolean
  // Provider channels: the provider, the module serving it and its routing priority.
  provider?: string
  module?: string
  priority?: number
}

// What a payment provider may do, as its module described it and a checker authorised it.
export type ProviderCapabilities = {
  name: string
  collections?: { method: string; currencies: string[]; hosted?: boolean }[]
  payouts?: { rail: string; currencies: string[] }[]
  deposits?: boolean
  bankCode?: string
  notificationValidation?: string
  settlementConfirmed?: boolean
  idempotentCharges?: boolean
  actions?: { name: string; label: string; description?: string }[]
  checkCharge?: boolean
  checkTransaction?: boolean
  requestStatus?: boolean
  balance?: boolean
  webhooks?: boolean
}

// provider names the payment provider of a provider channel and module the module serving it
// ('' and '' for a transfer channel; module '' with a provider is the sandbox simulator).
export type Channel = Omit<ChannelSpec, 'active'> & {
  id: string; version: number; code: string; provider: string; module: string; priority: number; capabilities: ProviderCapabilities
  inwardType: string; outwardType: string; active: boolean; createdAt: string
}

export type ChannelView = Channel & { accounts: LedgerAccount[]; openItems: number; failedEvents: number; pendingEvents: number; routable: boolean; notRoutable?: string }

export type ChannelTransfer = SuspenseItem & { channel: string; direction: 'Inward' | 'Outward'; outcome: string; providerReference: string }

export type KycReview = {
  id: string
  customerId: string
  legalName: string
  type: string
  email: string
  documentType: string
  documentNumber: string
  dateOfBirth: string | null
  registrationNumber: string | null
  directors: string[]
  submittedAt: string
}

export type CurrencyRate = { currency: string; perUsd: number; asOf: string; ageMinutes: number; stale: boolean }

export type Operator = {
  id: string
  version: number
  username: string
  name: string
  email: string
  permissions: ('maker' | 'checker' | 'admin' | 'analyst')[]
  active: boolean
  mustChangePassword: boolean
  passwordChangedAt: string | null
  failedLogins: number
  lockedUntil: string | null
  lastLoginAt: string | null
  createdAt: string
}

// ---- EB.LOCAL · local tables (LOCAL.TABLE / LOCAL.REF / local applications) ----

export type LocalFieldType = 'text' | 'number' | 'amount' | 'date' | 'boolean' | 'choice' | 'lookup'

export type LocalColumn = {
  name: string
  label: string
  type: LocalFieldType
  required?: boolean
  maxLength?: number
  scale?: number
  pattern?: string
  options?: string[]
  lookup?: string
  searchable?: boolean
  retired?: boolean
}

export type LocalTableSpec = {
  description: string
  kind: 'reference' | 'details' | 'application'
  keyLabel?: string
  keyPattern?: string
  authorisers: number
  fields: LocalColumn[]
}

export type PhysicalColumn = { tableName: string; name: string; columnName: string; status: 'pending' | 'applied' | 'failed'; error?: string; appliedAt?: string }

export type SchemaChange = { id: string; tableName: string; target: string; kind: string; statement: string; status: string; error?: string }

export type LocalTable = LocalTableSpec & {
  id: string
  version: number
  name: string
  tableName: string
  columns: PhysicalColumn[]
  changes: SchemaChange[]
  ready: boolean
  createdAt: string
  updatedAt: string
}

export type LocalRecord = { table: string; key: string; id: string; version: number; values: Record<string, string>; updatedAt?: string; updatedBy?: string }

// ---- FT · transactions, transaction types and versions ----

export type TransactionFlow = 'transfer' | 'collection' | 'payout' | 'refund' | 'channel_out' | 'deposit' | 'channel_in'

export type TransactionTypeSpec = {
  description: string
  flow: TransactionFlow
  narrative?: string
  reportingClass?: string
  currencies?: string[]
  minAmount?: number
  maxAmount?: number
  disabled?: boolean
}

export type TransactionType = Required<Omit<TransactionTypeSpec, 'disabled'>> & { id: string; version: number; code: string; direction: string; disabled: boolean }

export type VersionField = { field: string; label?: string; mandatory?: boolean; noInput?: boolean; hidden?: boolean; default?: string }

export type OverrideClass = 'accept' | 'checker' | 'refuse' | 'ignore'

export type VersionSpec = {
  description: string
  transactionType: string
  channels: string[]
  authorisers: number
  fields?: VersionField[]
  minAmount?: number
  maxAmount?: number
  currencies?: string[]
  inputRoutines?: string[]
  authRoutines?: string[]
  overrides?: Record<string, OverrideClass>
  reportingClass?: string
  disabled?: boolean
}

export type Version = Omit<VersionSpec, 'fields' | 'overrides'> & {
  id: string
  version: number
  name: string
  fields: VersionField[] | null
  overrides: Record<string, OverrideClass> | null
  /** The screen: the flow and the core and local fields it takes. */
  flow: TransactionFlow
  inputs: string[]
  localFields: LocalColumn[]
}

export type Override = { code: string; message: string }

export type Transaction = {
  id: string
  currNo: number
  reference: string
  recordStatus: 'INAU' | 'LIVE' | 'RJCT'
  status: string
  flowStatus: string
  transactionType: string
  version: string
  flow: TransactionFlow
  direction: string
  channel: string
  reportingClass: string
  amount: number
  currency: string
  debitAccount?: string
  debitAmount?: number
  creditAccount?: string
  creditAmount?: number
  chargeAmount: number
  bookingDate?: string
  valueDate?: string
  narrative?: string
  paymentDetails: string[]
  orderingName?: string
  orderingAccount?: string
  orderingPhone?: string
  orderingInstitution?: string
  beneficiaryName?: string
  beneficiaryAccount?: string
  beneficiaryPhone?: string
  beneficiaryInstitution?: string
  rail?: string
  provider?: string
  providerReference?: string
  flowRecordType?: string
  flowRecordId?: string
  overridesAccepted: string[]
  inputter: string
  authoriser?: string
  inputAt: string
  authorisedAt?: string
  declineCode?: string
  createdAt: string
}

export type TransactionView = Transaction & {
  local: Record<string, string>
  details?: Record<string, Record<string, string>>
  nextAction?: { type: string; url: string; fields: Record<string, string>; expiresAt: string }
  pendingOverrides?: Override[]
  entries?: { id: string; transactionReference: string; category: string; description: string; bookingDate: string; lines: { accountId: string; direction: string; amount: number; currency: string }[] }[]
  flow?: Record<string, unknown>
}

// ---- ENQUIRY ----

export type Operand = 'EQ' | 'NE' | 'GT' | 'GE' | 'LT' | 'LE' | 'LK' | 'RG' | 'IN'

export type EnquirySpec = {
  description: string
  application: string
  fixed?: { field: string; op: Operand; value: string }[]
  prompts?: { field: string; op: Operand; label?: string; mandatory?: boolean }[]
  columns: { field: string; label?: string }[]
  sort?: { field: string; desc?: boolean }[]
  maxRows?: number
}

export type Enquiry = {
  id: string
  version: number
  name: string
  description: string
  application: string
  criteria: { fixed?: EnquirySpec['fixed']; prompts?: EnquirySpec['prompts'] }
  output: { columns: EnquirySpec['columns']; sort?: EnquirySpec['sort'] }
  maxRows: number
}

export type EnquiryResult = { enquiry: string; columns: { field: string; label?: string }[]; rows: Record<string, unknown>[]; truncated: boolean }
