import { render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import MigrationsPage from './MigrationsPage';

const mockWatch = useK8sWatchResource as jest.Mock;

describe('MigrationsPage', () => {
  it('shows an empty state when there are no migrations', () => {
    mockWatch.mockReturnValue([[], true, undefined]);
    render(<MigrationsPage />);
    expect(screen.getByRole('heading', { name: 'Physical migrations' })).toBeInTheDocument();
    expect(screen.getByText('No physical migrations yet')).toBeInTheDocument();
  });

  it('lists migrations with their phase', () => {
    mockWatch.mockReturnValue([
      [
        {
          metadata: { uid: '1', name: 'web01', namespace: 'apps' },
          spec: { sourceRef: { name: 'web01-server' }, mode: 'cold' },
          status: { phase: 'Copying' },
        },
      ],
      true,
      undefined,
    ]);
    render(<MigrationsPage />);
    expect(screen.getByText('web01-server')).toBeInTheDocument();
    expect(screen.getByText('Copying')).toBeInTheDocument();
  });
});
