import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { Order, LogEntry, Deal } from './models';

export interface NewDeal {
  code: string;
  name: string;
  aisle: string;
  brand: string;
  piecesTotal: number;
  maxPerShopper: number;
  priceCents: number;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  deals = () => firstValueFrom(this.http.get<Deal[]>('/api/deals'));
  openDeal = (body: NewDeal) => firstValueFrom(this.http.post<Deal>('/api/deals', body));

  orders = (dealCode: string) =>
    firstValueFrom(this.http.get<Order[]>('/api/orders', { params: { dealCode } }));
  book = (body: { dealCode: string; reference: string; pieces: number; hold: boolean }) =>
    firstValueFrom(this.http.post<Order>('/api/orders', body));
  confirm = (id: string) => firstValueFrom(this.http.post<Order>(`/api/orders/${id}/confirm`, {}));
  refund = (id: string) => firstValueFrom(this.http.post<Order>(`/api/orders/${id}/refund`, {}));
  events = (id: string) => firstValueFrom(this.http.get<LogEntry[]>(`/api/orders/${id}/events`));
}
