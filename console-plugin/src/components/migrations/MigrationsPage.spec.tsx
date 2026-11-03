import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import MigrationsPage from './MigrationsPage';

const mockWatch = useK8sWatchResource as jest.Mock;
const renderPage = () =>
  render(
    <MemoryRouter>
      <MigrationsPage />
    </MemoryRouter>,
  );

describe('MigrationsPage', () => {
  it('shows an empty state when there are no migrations', () => {
    mockWatch.mockReturnValue([[], true, undefined]);
    renderPage();
    expect(screen.getByRole('heading', { name: 'Physical migrations' })).toBeInTheDocument();
    expect(screen.getByText('No physical migrations yet')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Migrate a server' })).toBeInTheDocument();
  });

  it('lists migrations with their phase and copy progress', () => {
    mockWatch.mockReturnValue([
      [
        {
          metadata: { uid: '1', name: 'web01', namespace: 'apps' },
          spec: { sourceRef: { name: 'web01-server' }, disks: [] },
          status: {
            phase: 'Copying',
            disks: [{ sourceDiskID: 'd', bytesTotal: 4, bytesCopied: 1 }],
          },
        },
      ],
      true,
      undefined,
    ]);
    renderPage();
    expect(screen.getByRole('link', { name: 'web01' })).toHaveAttribute(
      'href',
      '/p2v/ns/apps/migrations/web01',
    );
    expect(screen.getByText('Copying')).toBeInTheDocument();
    expect(screen.getByText('25%')).toBeInTheDocument();
  });
});
