import SwaggerParser from '@apidevtools/swagger-parser';
import { fileURLToPath } from 'node:url';

await SwaggerParser.validate(fileURLToPath(new URL('./openapi.json', import.meta.url)));
console.log('OpenAPI specification is valid');
