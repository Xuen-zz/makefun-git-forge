declare class Schema {
  private readonly types: unknown;
}

export function buildSchema(source: string): Schema;

export function graphqlSync(args: {
  schema: Schema;
  source: string;
  rootValue?: object;
  variableValues?: Record<string, unknown> | null;
  operationName?: string | null;
}): {
  data?: Record<string, unknown> | null;
  errors?: readonly Error[];
};
