# ADR 0023 — Customer onboarding: one request from the customer app, KYC required or optional by policy

**Status:** Accepted

## Context

Customers sign up in the customer app, which then had to make three calls to Zirnis:

1. `POST /customers`;
2. `POST /customers/{id}/kyc`;
3. `POST /accounts`.

A failure between two calls left a half-onboarded customer. A retry then hit `customer_exists`.

Whether KYC must be given at sign-up is a business decision that changes over time. A low-limit wallet before KYC suits some products and regulators; others need KYC first. It should be data that administrators control, not a release.

Customers do not call Zirnis themselves. Zirnis is internal ([ADR 0022](0022-payer-edge.md)), and the app holds the sign-in, the device and the customer's consent.

## Decision

**`POST /onboarding`** (platform API, `Idempotency-Key` required) takes three parts:

- the customer;
- their KYC or KYB submission;
- the currencies of their accounts (none means one account in the local currency; at most 10, no repeats).

It runs in this order:

1. **Check everything before writing anything:**
   - the policy;
   - the currencies;
   - KYB's registration number;
   - that the email is not already a customer's.
2. **Ask the KYC vendor**, outside any unit of work, about the customer as they will be recorded (the same id).
3. **Write in one unit of work:** the customer, the vendor's answer (a decision, or a submission waiting for operations' review) and the accounts.

A failure anywhere writes nothing, so the app retries the whole request, and the key replays a success.

The answer is the customer, including `kycStatus`, and their accounts.

**The onboarding policy** is one parameter record, `OnboardingPolicy`, with `kycRequired` and a reason.

- An administrator inputs a change: `POST /ops/onboarding-policy`, or **Operators → Customer onboarding** in the console.
- A different operator with the checker permission authorises it.
- Its history is kept ($HIS).
- Until one is authorised, KYC is **required**.
- `GET /onboarding/policy` tells the app which sign-up form to show.

| | KYC required | KYC optional |
|---|---|---|
| Sign-up without KYC | refused, `kyc_required` | accepted; the customer is `NotStarted` |
| The vendor rejects KYC | refused, `kyc_rejected`; nothing is kept but the audit event | the customer is kept as `Rejected`, with their accounts, and may resubmit |
| KYC under review (production: manual) | accepted; the customer is `Pending` | accepted |
| `POST /accounts` for a customer who has not submitted KYC, or was rejected | refused, `kyc_required` | accepted |

The policy decides who gets an account. It does not decide what an account may do. Moving money still needs a verified customer, and tiers set the limits.

## Consequences

- One call from the app, with no half-created customers.
- Changing the policy needs two people and leaves a trail. The policy applies to the separate calls too, so it cannot be bypassed by calling them one at a time.
- The three separate endpoints remain for the console and for resubmitting KYC.
- A sign-up the vendor rejects under a required policy leaves no customer record, only the audit event `onboarding.kyc_rejected`. The app should say so to the customer.
