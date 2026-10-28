import { test, expect, Page } from '@playwright/test';
import { ADMIN, signIn } from './support';

const run = () => Date.now().toString(36).toUpperCase() + Math.floor(Math.random() * 1000);

async function openAccount(
  page: Page,
  code: string,
  capacity: number,
  shown: string,
  limit = capacity,
) {
  await signIn(page, ADMIN);
  await page.getByLabel('Deal code').fill(code);
  await page.getByLabel('Pieces on sale').fill(String(capacity));
  await page.getByLabel('Limit per shopper').fill(String(limit));
  await page.getByRole('button', { name: 'Add deal' }).click();
  await expect(page.getByRole('heading', { name: `Buy pieces for ${code}` })).toBeVisible();
  await expect(page.getByTestId('selected-remaining')).toHaveText(shown);
}

async function submit(page: Page, reference: string, quantity: number) {
  await page.getByLabel('Shopper reference').fill(reference);
  await page.getByLabel('Pieces', { exact: true }).fill(String(quantity));
  await page.getByRole('button', { name: 'Buy' }).click();
}

const row = (page: Page, reference: string) => page.locator(`tr[data-reference="${reference}"]`);

test('the API refuses bad input and the page says why', async ({ page }) => {
  const id = run();
  await openAccount(page, `UI-ERR-${id}`, 5, '5');

  await submit(page, `SHP-${id}`, 7);
  await expect(page.getByRole('alert')).toContainText('at most 6 pieces');

  await submit(page, ' ', 1);
  await expect(page.getByRole('alert')).toContainText(/reference is required/i);

  await submit(page, `SHP-${id}`, 1);
  await expect(row(page, `SHP-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
  await submit(page, `SHP-${id}`, 1);
  await expect(page.getByRole('alert')).toContainText('already');
  await expect(page.locator('tbody tr[data-reference]')).toHaveCount(1);
});

test('a poison reference is rejected, gets no document, and the amount comes back', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-POI-${id}`, 5, '5');

  await submit(page, `SHP-POISON-${id}`, 2);
  const poisoned = row(page, `SHP-POISON-${id}`);
  await expect(poisoned.getByTestId('status')).toHaveText('Rejected', { timeout: 20_000 });
  await expect(poisoned).toContainText('poison');
  await expect(poisoned.getByTestId('receipt')).toHaveText('—');
  await expect(poisoned.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');
});

test('the last piece goes to one request and the same shopper asking again is held to the limit', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-LAST-${id}`, 1, '1');

  await submit(page, `SHP-A-${id}`, 1);
  await submit(page, `SHP-B-${id}`, 1);

  const statuses = page.locator('tbody tr[data-reference] [data-testid="status"]');
  await expect(statuses).toHaveCount(2);
  await expect
    .poll(async () => (await statuses.allTextContents()).map((s) => s.trim()).sort(), {
      timeout: 20_000,
    })
    .toEqual(['Sold', 'Rejected'].sort());
  await expect(page.locator('tbody')).toContainText('per shopper');
  await expect(page.getByTestId('selected-remaining')).toHaveText('0');
});

test('the event log shows the Kafka steps and the reversal keeps the document', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-LOG-${id}`, 4, '4');

  await submit(page, `SHP-${id}`, 2);
  const mine = row(page, `SHP-${id}`);
  await expect(mine.getByTestId('status')).toHaveText('Sold', { timeout: 20_000 });

  await mine.getByRole('button', { name: 'Event log' }).click();
  const log = page.getByRole('region', { name: 'Kafka event log' });
  await expect(log).toContainText('pieces.reserve', { timeout: 20_000 });
  await expect(log).toContainText('pieces.held');
  await expect(log).toContainText('receipt.issued');

  await mine.getByRole('button', { name: 'Refund' }).click();
  await expect(mine.getByTestId('status')).toHaveText('Refunded');
  await expect(mine.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(mine.getByTestId('receipt')).toHaveText(/FL-[0-9A-F]{12}/);
  await expect(page.getByTestId('selected-remaining')).toHaveText('4');
  await expect(log).toContainText('pieces.released', { timeout: 20_000 });
});

test('one shopper cannot go past the limit per shopper, and a refund gives the allowance back', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-CAP-${id}`, 10, '10', 2);
  await expect(page.getByTestId(`remaining-UI-CAP-${id}`)).toContainText('max 2 each');

  await submit(page, `SHP-ONE-${id}`, 2);
  await expect(row(page, `SHP-ONE-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });

  await submit(page, `SHP-TWO-${id}`, 1);
  const capped = row(page, `SHP-TWO-${id}`);
  await expect(capped.getByTestId('status')).toHaveText('Rejected', { timeout: 20_000 });
  await expect(capped).toContainText('at most 2 pieces per shopper');
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');

  await row(page, `SHP-ONE-${id}`).getByRole('button', { name: 'Refund' }).click();
  await expect(row(page, `SHP-ONE-${id}`).getByTestId('status')).toHaveText('Refunded', {
    timeout: 20_000,
  });
  await expect(page.getByTestId('selected-remaining')).toHaveText('10');

  await submit(page, `SHP-THREE-${id}`, 2);
  await expect(row(page, `SHP-THREE-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
});

test('a held order can be confirmed before its hold runs out, and then it sells', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-HOLD-${id}`, 5, '5');
  await page.getByLabel(/Hold these pieces and pay later/).check();

  await submit(page, `SHP-HOLD-${id}`, 2);
  const mine = row(page, `SHP-HOLD-${id}`);
  await expect(mine.getByTestId('status')).toHaveText('Held', { timeout: 20_000 });
  await expect(mine.getByTestId('expires')).toBeVisible();
  await expect(mine.getByTestId('receipt')).toHaveText('—'); // nothing is issued while the hold is pending
  await expect(page.getByTestId('selected-remaining')).toHaveText('3');

  await mine.getByRole('button', { name: 'Confirm' }).click();
  await expect(mine.getByTestId('status')).toHaveText('Sold', { timeout: 20_000 });
  await expect(mine.getByTestId('receipt')).toHaveText(/FL-[0-9A-F]{12}/);
  await expect(page.getByTestId('selected-remaining')).toHaveText('3');
});

// Compose gives holds 12 seconds (Deals__HoldSeconds); against the Nx dev servers the default is 10 minutes.
test('an unconfirmed hold expires by itself and the pieces go back on sale', async ({ page }) => {
  test.skip(!!process.env.WEB_URL, 'needs the short hold time the Compose stack sets');
  const id = run();
  await openAccount(page, `UI-EXP-${id}`, 5, '5');
  await page.getByLabel(/Hold these pieces and pay later/).check();

  await submit(page, `SHP-EXP-${id}`, 3);
  const mine = row(page, `SHP-EXP-${id}`);
  await expect(mine.getByTestId('status')).toHaveText('Held', { timeout: 20_000 });
  await expect(page.getByTestId('selected-remaining')).toHaveText('2');

  await expect(mine.getByTestId('status')).toHaveText('Expired', { timeout: 40_000 });
  await expect(mine).toContainText('hold expired');
  await expect(mine.getByRole('button', { name: 'Confirm' })).toHaveCount(0);
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');
});
