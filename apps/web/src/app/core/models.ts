export interface Deal {
  id: string;
  code: string;
  name: string;
  aisle: string;
  brand: string;
  saleDate: string;
  priceCents: number;
  piecesTotal: number;
  piecesLeft: number;
  maxPerShopper: number;
}

export type OrderStatus = 'Requested' | 'Held' | 'Sold' | 'Refunded' | 'Rejected' | 'Expired';

export interface Order {
  id: string;
  dealCode: string;
  ownerId: string;
  reference: string;
  pieces: number;
  totalCents: number;
  status: OrderStatus;
  reason: string | null;
  receiptNumber: string | null;
  createdAt: string;
  updatedAt: string;
  piecesReleasedAt: string | null;
  holdExpiresAt: string | null;
}

export interface LogEntry {
  id: string;
  type: string;
  module: string;
  occurredAt: string;
}

export interface UserInfo {
  id: string;
  email: string;
  roles: string[];
}

export interface AuthResponse {
  accessToken: string;
  expiresAt: string;
  user: UserInfo;
}
